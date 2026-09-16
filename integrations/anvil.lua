-- LazyVim integration for anvil dev VMs.
--
-- Commands:
--   :AnvilDevCreate  create a dev VM for the current project
--   :AnvilDevShell   shell into it
--   :AnvilDevPurge   delete + purge it
--   :AnvilDevList    list dev VMs spawned for the current project
--

local M = {}

local IMAGES = {
	{ id = "ubuntu-26.04", pkgmgr = "apt", user = "ubuntu" },
	{ id = "almalinux-9", pkgmgr = "dns", user = "almalinux" },
	{ id = "debian-13", pkgmgr = "apt", user = "debian" },
	{ id = "archlinux", pkgmgr = "pacman", user = "arch" },
	{ id = "fedora-44", pkgmgr = "dnf", user = "fedora" },
}

local LANGUAGES = {
	{ marker = "go.mod", apt = "golang-go", pacman = "go", dnf = "golang" },
	{ marker = "Cargo.toml", apt = "rustc cargo", pacman = "rust", dnf = "rust cargo" },
	{ marker = "package.json", apt = "nodejs npm", pacman = "nodejs npm", dnf = "nodejs npm" },
	{
		marker = "pyproject.toml",
		apt = "python3 python3-pip",
		pacman = "python python-pip",
		dnf = "python3 python3-pip",
	},
	{
		marker = "requirements.txt",
		apt = "python3 python3-pip",
		pacman = "python python-pip",
		dnf = "python3 python3-pip",
	},
	{ marker = "Gemfile", apt = "ruby-full", pacman = "ruby", dnf = "ruby" },
	{ marker = "composer.json", apt = "php", pacman = "php", dnf = "php" },
	{ marker = "pom.xml", apt = "default-jdk maven", pacman = "jdk-openjdk maven", dnf = "java-latest-openjdk maven" },
	{
		marker = "build.gradle",
		apt = "default-jdk gradle",
		pacman = "jdk-openjdk gradle",
		dnf = "java-latest-openjdk gradle",
	},
}

local INSTALL_CMD = {
	apt = "export DEBIAN_FRONTEND=noninteractive; apt-get update -y && apt-get install -y fish %s",
	pacman = "pacman -Sy --noconfirm fish %s",
	dnf = "dnf install -y fish %s",
}

local function slug(s)
	s = s:lower():gsub("[^%w]+", "-"):gsub("^%-+", ""):gsub("%-+$", "")
	return s ~= "" and s or "project"
end

local function project_root()
	return vim.fn.getcwd()
end

local function project_slug()
	return slug(vim.fn.fnamemodify(project_root(), ":t"))
end

local function detect_language()
	for _, entry in ipairs(LANGUAGES) do
		if vim.fn.filereadable(project_root() .. "/" .. entry.marker) == 1 then
			return entry
		end
	end
	return nil
end

local function cloud_init(image, lang)
	local pkgs = lang and lang[image.pkgmgr] or ""
	local install = string.format(INSTALL_CMD[image.pkgmgr], pkgs)
	return table.concat({
		"#cloud-config",
		"runcmd:",
		"  - " .. install,
		string.format('  - chsh -s "$(command -v fish)" %s', image.user),
	}, "\n") .. "\n"
end

local function list_instances(cb)
	local prefix = "dev-" .. project_slug()
	vim.system({ "anvil", "list" }, { text = true }, function(res)
		local names = {}
		if res.code == 0 and res.stdout then
			for line in res.stdout:gmatch("[^\n]+") do
				local name = line:match("^(%S+)")
				if name and name ~= "NAME" and name:sub(1, #prefix) == prefix then
					table.insert(names, name)
				end
			end
		end
		vim.schedule(function()
			cb(names)
		end)
	end)
end

local function next_name(cb)
	list_instances(function(names)
		local taken = {}
		for _, n in ipairs(names) do
			taken[n] = true
		end
		local prefix = "dev-" .. project_slug()
		local name, i = prefix, 2
		while taken[name] do
			name = prefix .. "-" .. i
			i = i + 1
		end
		cb(name)
	end)
end

local WAIT_TRIES = 30
local SPINNER_FRAMES = { "⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏" }

local function progress_bar(pct, width)
	width = width or 20
	local filled = math.min(width, math.floor(pct * width + 0.5))
	return string.rep("█", filled) .. string.rep("░", width - filled)
end

-- Spinner for phases with no known duration (launch, copy). Bounded phases
-- (SSH wait) use progress_bar instead, since we know the attempt count.
-- Notifications are updated in place via a stable `id`: passing the previous
-- return value as `replace` (nvim-notify style) doesn't reuse the window in
-- Snacks' notifier, it only keys off `opts.id`.
local function spinner_start(msg, id)
	local frame = 0
	local timer = (vim.uv or vim.loop).new_timer()
	timer:start(
		0,
		120,
		vim.schedule_wrap(function()
			frame = (frame % #SPINNER_FRAMES) + 1
			vim.notify(
				string.format("%s %s", msg, SPINNER_FRAMES[frame]),
				vim.log.levels.INFO,
				{ id = id, title = "anvil" }
			)
		end)
	)
	return {
		stop = function()
			timer:stop()
			timer:close()
		end,
	}
end

local function copy_project(name, id)
	local root = project_root()
	local remote_dir = project_slug()
	local spin = spinner_start(string.format("anvil: copying project into %s:~/%s", name, remote_dir), id)
	vim.system({ "anvil", "transfer", root, name .. ":~/" .. remote_dir }, { text = true }, function(res)
		vim.schedule(function()
			spin.stop()
			if res.code == 0 then
				vim.notify("anvil: " .. name .. " ready", vim.log.levels.INFO, { id = id, title = "anvil" })
			else
				vim.notify(
					"anvil: copy into " .. name .. " failed: " .. (res.stderr or ""),
					vim.log.levels.ERROR,
					{ id = id, title = "anvil" }
				)
			end
		end)
	end)
end

local function wait_for_ssh(name, id, attempt)
	attempt = attempt or 1
	vim.system({ "anvil", "exec", name, "--", "true" }, { text = true }, function(res)
		if res.code == 0 then
			vim.schedule(function()
				copy_project(name, id)
			end)
		elseif attempt < WAIT_TRIES then
			vim.schedule(function()
				vim.notify(
					string.format("anvil: booting %s %s", name, progress_bar(attempt / WAIT_TRIES)),
					vim.log.levels.INFO,
					{ id = id, title = "anvil" }
				)
			end)
			vim.defer_fn(function()
				wait_for_ssh(name, id, attempt + 1)
			end, 2000)
		else
			vim.schedule(function()
				vim.notify(
					"anvil: " .. name .. " never became reachable over SSH, skipping project copy",
					vim.log.levels.WARN,
					{ id = id, title = "anvil" }
				)
			end)
		end
	end)
end

function M.create()
	local image_ids = {}
	for _, im in ipairs(IMAGES) do
		table.insert(image_ids, im.id)
	end

	vim.ui.select(image_ids, { prompt = "anvil: base image" }, function(choice)
		if not choice then
			return
		end
		local image
		for _, im in ipairs(IMAGES) do
			if im.id == choice then
				image = im
			end
		end

		vim.ui.input({ prompt = "vCPUs: ", default = "2" }, function(cpus)
			if not cpus then
				return
			end
			vim.ui.input({ prompt = "Memory (MiB): ", default = "2048" }, function(mem)
				if not mem then
					return
				end
				vim.ui.input({ prompt = "Disk (GiB): ", default = "10" }, function(disk)
					if not disk then
						return
					end

					next_name(function(name)
						local ci_path = vim.fn.tempname() .. ".yaml"
						vim.fn.writefile(vim.split(cloud_init(image, detect_language()), "\n"), ci_path)

						local launch_cmd = {
							"anvil",
							"launch",
							image.id,
							"--kind",
							"vm",
							"--name",
							name,
							"--cpus",
							cpus,
							"--memory",
							mem,
							"--disk",
							disk,
							"--cloud-init",
							ci_path,
						}

						local id = "anvil:" .. name
						local spin = spinner_start("anvil: launching " .. name, id)
						vim.system(launch_cmd, { text = true }, function(res)
							os.remove(ci_path)
							vim.schedule(function()
								spin.stop()
								if res.code ~= 0 then
									vim.notify(
										"anvil: launch failed for " .. name .. ": " .. (res.stderr or ""),
										vim.log.levels.ERROR,
										{ id = id, title = "anvil" }
									)
									return
								end
								wait_for_ssh(name, id)
							end)
						end)
					end)
				end)
			end)
		end)
	end)
end

function M.shell()
	list_instances(function(names)
		if #names == 0 then
			vim.notify("anvil: no dev instance for this project yet, run :AnvilDevCreate", vim.log.levels.WARN)
			return
		end
		local function open(name)
			-- Snacks defaults to a centered float whenever a `cmd` is given;
			-- <leader>ft passes no cmd and gets the bottom split, so force it here too.
			require("snacks").terminal({ "anvil", "shell", name }, { win = { position = "bottom" } })
		end
		if #names == 1 then
			open(names[1])
		else
			vim.ui.select(names, { prompt = "anvil: shell into" }, function(choice)
				if choice then
					open(choice)
				end
			end)
		end
	end)
end

function M.purge()
	list_instances(function(names)
		if #names == 0 then
			vim.notify("anvil: no dev instance for this project", vim.log.levels.WARN)
			return
		end
		local function do_purge(name)
			vim.ui.select({ "Yes", "No" }, { prompt = "anvil: purge " .. name .. "?" }, function(choice)
				if choice ~= "Yes" then
					return
				end
				vim.system({ "anvil", "delete", name, "--purge" }, { text = true }, function(res)
					vim.schedule(function()
						if res.code == 0 then
							vim.notify("anvil: purged " .. name)
						else
							vim.notify(
								"anvil: purge of " .. name .. " failed: " .. (res.stderr or ""),
								vim.log.levels.ERROR
							)
						end
					end)
				end)
			end)
		end
		if #names == 1 then
			do_purge(names[1])
		else
			vim.ui.select(names, { prompt = "anvil: purge which" }, function(choice)
				if choice then
					do_purge(choice)
				end
			end)
		end
	end)
end

function M.list()
	list_instances(function(names)
		if #names == 0 then
			vim.notify("anvil: no dev instances for this project")
			return
		end
		vim.notify("anvil dev instances:\n  " .. table.concat(names, "\n  "))
	end)
end

return {
	{
		"anvil-dev",
		dir = vim.fn.fnamemodify(debug.getinfo(1, "S").source:sub(2), ":h"),
		lazy = false,
		config = function()
			if vim.fn.executable("anvil") == 0 then
				vim.notify("anvil.lua: `anvil` not found on $PATH", vim.log.levels.WARN)
			end

			vim.api.nvim_create_user_command("AnvilDevCreate", M.create, {})
			vim.api.nvim_create_user_command("AnvilDevShell", M.shell, {})
			vim.api.nvim_create_user_command("AnvilDevPurge", M.purge, {})
			vim.api.nvim_create_user_command("AnvilDevList", M.list, {})

			vim.keymap.set("n", "<leader>Ac", M.create, { desc = "Anvil: create dev VM" })
			vim.keymap.set("n", "<leader>As", M.shell, { desc = "Anvil: shell into dev VM" })
			vim.keymap.set("n", "<leader>Ap", M.purge, { desc = "Anvil: purge dev VM" })
			vim.keymap.set("n", "<leader>Al", M.list, { desc = "Anvil: list dev VMs" })
		end,
	},
	{
		"folke/which-key.nvim",
		optional = true,
		opts = function(_, opts)
			opts.spec = opts.spec or {}
			vim.list_extend(opts.spec, {
				{ "<leader>A", group = "anvil", icon = "🔨" },
			})
		end,
	},
}
