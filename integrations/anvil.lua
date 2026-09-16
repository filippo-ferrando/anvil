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
	{ id = "ubuntu-24.04", pkgmgr = "apt", user = "ubuntu" },
	{ id = "debian-12", pkgmgr = "apt", user = "debian" },
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

local function open_terminal(cmd, on_exit)
	vim.cmd("botright new")
	local win = vim.api.nvim_get_current_win()

	local function close_and_forward(job_id, code, event)
		if on_exit then
			on_exit(job_id, code, event)
		end
		vim.schedule(function()
			if vim.api.nvim_win_is_valid(win) then
				vim.api.nvim_win_close(win, true)
			end
		end)
	end

	vim.fn.termopen(cmd, { on_exit = close_and_forward })
	vim.cmd("startinsert")
end

local function copy_project(name)
	local root = project_root()
	local remote_dir = project_slug()
	vim.notify(string.format("anvil: copying %s into %s:~/%s", root, name, remote_dir))
	vim.system({ "anvil", "transfer", root, name .. ":~/" .. remote_dir }, { text = true }, function(res)
		vim.schedule(function()
			if res.code == 0 then
				vim.notify("anvil: project copied into " .. name)
			else
				vim.notify("anvil: copy into " .. name .. " failed: " .. (res.stderr or ""), vim.log.levels.ERROR)
			end
		end)
	end)
end

local function wait_for_ssh(name, tries)
	tries = tries or 30
	vim.system({ "anvil", "exec", name, "--", "true" }, { text = true }, function(res)
		if res.code == 0 then
			vim.schedule(function()
				copy_project(name)
			end)
		elseif tries > 1 then
			vim.defer_fn(function()
				wait_for_ssh(name, tries - 1)
			end, 2000)
		else
			vim.schedule(function()
				vim.notify(
					"anvil: " .. name .. " never became reachable over SSH, skipping project copy",
					vim.log.levels.WARN
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

						open_terminal(launch_cmd, function(_, code)
							os.remove(ci_path)
							if code ~= 0 then
								vim.notify("anvil: launch failed for " .. name, vim.log.levels.ERROR)
								return
							end
							wait_for_ssh(name)
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
			open_terminal({ "anvil", "shell", name })
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
}
