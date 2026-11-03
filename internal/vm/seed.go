//go:build linux

package vm

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/cloudinit"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
)

// buildSeed (re)builds spec's cloud-init seed ISO from its current
// user-data, SSH keys, and mounts, overwriting v.SeedISOPath.
func (b *Backend) buildSeed(spec *instance.Spec) error {
	v := spec.VM
	dir := instanceDir(spec.ID)

	userData := v.CloudInitUserData
	if userData == "" && v.CloudInitName != "" {
		cfg, err := b.Source.GetCloudInit(v.CloudInitName)
		if err != nil {
			return fmt.Errorf("vm: resolving cloud-init config %q: %w", v.CloudInitName, err)
		}
		userData = cfg.Content
	}
	userData, err := seedUserData(v, userData)
	if err != nil {
		return fmt.Errorf("vm: preparing cloud-init user-data: %w", err)
	}

	// Bumping Generation into instance-id forces cloud-init to treat a
	// reconfiguration as a new instance and re-run every module.
	metaData := fmt.Sprintf("instance-id: %s-gen%d\nlocal-hostname: %s\n", spec.ID, v.Generation, spec.Name)

	seedPath := filepath.Join(dir, "seed.iso")
	seed := cloudinit.Seed{UserData: userData, MetaData: metaData, NetworkConfig: bridgeNetworkConfig(v, v.MACAddress)}
	if err := b.Seed.Build(seed, seedPath); err != nil {
		return fmt.Errorf("vm: building cloud-init seed: %w", err)
	}
	v.SeedISOPath = seedPath
	return nil
}

// seedUserData adds anvil's own setup to userData: SSH keys, mounts, extra hosts, the guest
// agent and Firecracker's needs. With nothing to add userData is kept as it is.
func seedUserData(v *instance.VMSpec, userData string) (string, error) {
	fc := v.HypervisorOf() == instance.VMHypervisorFirecracker
	if len(v.SSHPublicKeys) == 0 && len(v.Mounts) == 0 && v.Generation == 0 && len(v.ExtraHosts) == 0 && v.NoGuestAgent && !fc {
		if userData == "" {
			return "#cloud-config\n{}\n", nil
		}
		return userData, nil
	}
	doc, err := parseCloudConfig(userData)
	if err != nil {
		return "", err
	}
	addSSHKeys(doc, v.SSHPublicKeys)
	line := fstabLine
	if fc {
		line = func(m instance.Mount) string { return nfsFstabLine(v.Gateway, m) }
	}
	addMounts(doc, v.Mounts, v.Generation, line)
	addExtraHosts(doc, v.ExtraHosts)
	if !v.NoGuestAgent {
		addGuestAgent(doc)
	}
	if fc {
		addFirecrackerGuest(doc, !v.NoGuestAgent)
		if v.AnvilGuest {
			addAnvilGuest(doc)
		}
	}
	return encodeCloudConfig(doc)
}

// addSSHKeys appends keys to doc's top-level ssh_authorized_keys list.
func addSSHKeys(doc map[string]any, keys []string) {
	if len(keys) == 0 {
		return
	}
	existing, _ := doc["ssh_authorized_keys"].([]any)
	for _, k := range keys {
		existing = append(existing, k)
	}
	doc["ssh_authorized_keys"] = existing
}

// shQuote single-quotes s for safe interpolation into a POSIX shell command.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// addExtraHosts adds one /etc/hosts entry per host via idempotent
// bootcmd lines, sorted by name for deterministic output.
func addExtraHosts(doc map[string]any, hosts map[string]string) {
	if len(hosts) == 0 {
		return
	}
	bootcmd, _ := doc["bootcmd"].([]any)
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		quoted := shQuote(fmt.Sprintf("%s %s", hosts[name], name))
		bootcmd = append(bootcmd, fmt.Sprintf(`grep -qxF %s /etc/hosts || echo %s >> /etc/hosts`, quoted, quoted))
	}
	doc["bootcmd"] = bootcmd
}

// parseCloudConfig decodes a "#cloud-config" user-data document; empty input gives an empty map.
func parseCloudConfig(userData string) (map[string]any, error) {
	body := strings.TrimPrefix(userData, "#cloud-config\n")
	doc := map[string]any{}
	if strings.TrimSpace(body) != "" {
		if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
			return nil, fmt.Errorf("parsing existing cloud-init user-data: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func encodeCloudConfig(doc map[string]any) (string, error) {
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("re-encoding cloud-init user-data: %w", err)
	}
	return "#cloud-config\n" + string(out), nil
}

// guestAgentPackage is the qemu-guest-agent package name, the same on every catalog distro.
const guestAgentPackage = "qemu-guest-agent"

// guestAgentStartCmd starts the agent right after install: the udev rule that normally starts it
// only fires on boot. The OpenRC branch covers Alpine.
const guestAgentStartCmd = "(systemctl enable qemu-guest-agent 2>/dev/null; systemctl start qemu-guest-agent) || " +
	"(rc-update add qemu-guest-agent default && rc-service qemu-guest-agent start) || true"

// addGuestAgent adds qemu-guest-agent to doc's "packages" and a runcmd that starts it.
func addGuestAgent(doc map[string]any) {
	packages, _ := doc["packages"].([]any)
	if !slices.Contains(packages, any(guestAgentPackage)) {
		packages = append(packages, guestAgentPackage)
	}
	doc["packages"] = packages
	runcmd, _ := doc["runcmd"].([]any)
	doc["runcmd"] = append(runcmd, guestAgentStartCmd)
}

// fcAgentPort is the guest vsock port qemu-guest-agent listens on in a Firecracker VM.
const fcAgentPort = 1024

// fcAgentUnit runs qemu-guest-agent on vsock: the distro's own unit only starts with the
// virtio-serial port QEMU provides.
var fcAgentUnit = fmt.Sprintf(`[Unit]
Description=QEMU guest agent on vsock (anvil)

[Service]
ExecStart=/bin/sh -c 'exec qemu-ga --method=vsock-listen --path=%d:%d'
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
`, firecracker.GuestCID, fcAgentPort)

// fcAgentStartCmd enables the vsock unit, or on OpenRC (Alpine) points the stock service at vsock.
const fcAgentStartCmd = "(systemctl daemon-reload && systemctl enable --now anvil-guest-agent) || " +
	"(rc-update add qemu-guest-agent default && rc-service qemu-guest-agent restart) || true"

// fcPoweroffCmd makes a systemd guest's poweroff a reboot: on x86_64 Firecracker only
// exits on a guest reboot, while a poweroff leaves its process running.
const fcPoweroffCmd = "[ -d /run/systemd/system ] && ln -sf /usr/lib/systemd/system/reboot.target /etc/systemd/system/poweroff.target || true"

// addFirecrackerGuest adds what a Firecracker guest needs on top of the common user-data:
// poweroff as reboot, and with agent the guest agent moved to vsock.
func addFirecrackerGuest(doc map[string]any, agent bool) {
	bootcmd, _ := doc["bootcmd"].([]any)
	doc["bootcmd"] = append(bootcmd, fcPoweroffCmd)
	if agent {
		files, _ := doc["write_files"].([]any)
		doc["write_files"] = append(files,
			map[string]any{"path": "/etc/systemd/system/anvil-guest-agent.service", "content": fcAgentUnit},
			map[string]any{"path": "/etc/conf.d/qemu-guest-agent",
				"content": fmt.Sprintf("GA_METHOD=\"vsock-listen\"\nGA_PATH=\"%d:%d\"\n", firecracker.GuestCID, fcAgentPort)},
		)
		runcmd, _ := doc["runcmd"].([]any)
		// The stock start command is for virtio-serial; this one replaces it.
		runcmd = slices.DeleteFunc(runcmd, func(c any) bool { return c == guestAgentStartCmd })
		doc["runcmd"] = append(runcmd, fcAgentStartCmd)
	}
}

// bridgeNetworkConfig returns a cloud-init network-config statically assigning
// v.StaticIP/v.Gateway to mac, or "" when v has no static address (SLIRP).
func bridgeNetworkConfig(v *instance.VMSpec, mac string) string {
	if v.StaticIP == "" {
		return ""
	}
	cfg := fmt.Sprintf(`network:
  version: 2
  ethernets:
    anvil0:
      match:
        macaddress: "%s"
      dhcp4: false
      addresses: [%s]
      gateway4: %s
`, mac, v.StaticIP, v.Gateway)
	if len(v.DNSServers) > 0 {
		cfg += fmt.Sprintf("      nameservers:\n        addresses: [%s]\n", strings.Join(v.DNSServers, ", "))
		if len(v.DNSSearch) > 0 {
			cfg += fmt.Sprintf("        search: [%s]\n", strings.Join(v.DNSSearch, ", "))
		}
	}
	return cfg
}
