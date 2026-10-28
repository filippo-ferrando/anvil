package cloudinit

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"git.fferrando.cc/rdfilippo/anvil/internal/yamlcheck"
)

// Problem is one finding in user-data.
type Problem = yamlcheck.Problem

// headers cloud-init accepts besides #cloud-config; their bodies aren't checked.
var otherHeaders = []string{"#!", "#include", "#cloud-boothook", "#part-handler", "#upstart-job", "## template: jinja", "Content-Type:"}

// knownKeys are the top-level keys of cloud-init's config schema.
// ponytail: hand-kept list, embed schema-cloud-config-v1.json if it drifts.
var knownKeys = toSet(`ansible apk_repos apt apt_pipelining apt_reboot_if_required apt_update apt_upgrade
apt_get_command apt_get_upgrade_subcommand apt_get_wrapper apt_mirror apt_mirror_search apt_sources
autoinstall bootcmd byobu_by_default ca_certs ca-certs chef chpasswd cloud_config_modules
cloud_final_modules cloud_init_modules create_hostname_file datasource datasource_list device_aliases
disable_ec2_metadata disable_root disable_root_opts disk_setup drivers fan final_message fqdn fs_setup
groups growpart grub_dpkg grub-dpkg hostname keyboard keys_to_console landscape launch-index locale
locale_configfile lxd manage_etc_hosts manage_resolv_conf manual_cache_clean mcollective merge_how
merge_type mount_default_fields mounts network no_ssh_fingerprints ntp output package_reboot_if_required
package_update package_upgrade packages password phone_home power_state prefer_fqdn_over_hostname
preserve_hostname puppet random_seed reporting resize_rootfs resolv_conf rh_subscription rsyslog runcmd
salt_minion snap spacewalk ssh ssh_authorized_keys ssh_deletekeys ssh_fp_console_blacklist
ssh_genkeytypes ssh_import_id ssh_key_console_blacklist ssh_keys ssh_publish_hostkeys ssh_pwauth
ssh_quiet_keygen swap system_info timezone ubuntu_advantage ubuntu_pro updates user users vendor_data
version wireguard write_files yum_repo_dir yum_repos zypper`)

// keyKinds holds the YAML kind a key must have, for keys with only one valid shape.
var keyKinds = map[string]yaml.Kind{}

func init() {
	for k := range toSet("packages runcmd bootcmd write_files ssh_authorized_keys ssh_import_id mounts fs_setup") {
		keyKinds[k] = yaml.SequenceNode
	}
	for k := range toSet("apt ntp ssh power_state chpasswd growpart phone_home ubuntu_pro ca_certs resolv_conf keyboard") {
		keyKinds[k] = yaml.MappingNode
	}
	for k := range toSet("package_update package_upgrade package_reboot_if_required disable_root preserve_hostname ssh_deletekeys ssh_quiet_keygen hostname fqdn timezone final_message") {
		keyKinds[k] = yaml.ScalarNode
	}
}

var boolKeys = toSet("package_update package_upgrade package_reboot_if_required disable_root preserve_hostname ssh_deletekeys ssh_quiet_keygen")

// Validate checks user-data the way cloud-init reads it: the header, the YAML,
// then top-level keys and the shape of the common ones.
func Validate(userData string) []Problem {
	first, _, _ := strings.Cut(userData, "\n")
	first = strings.TrimSpace(first)
	if !strings.HasPrefix(first, "#cloud-config") {
		for _, h := range otherHeaders {
			if strings.HasPrefix(first, h) {
				return nil
			}
		}
		return []Problem{{Line: 1, Message: "first line must be #cloud-config (or #!, #include, ...)"}}
	}

	if p := yamlcheck.Syntax(userData); p != nil {
		return []Problem{*p}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(userData), &doc); err != nil {
		return []Problem{{Message: err.Error()}}
	}
	if len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return []Problem{{Line: root.Line, Message: "cloud-config must be a mapping of keys"}}
	}

	var out []Problem
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i], root.Content[i+1]
		if !knownKeys[key.Value] {
			out = append(out, Problem{Line: key.Line, Message: fmt.Sprintf("unknown key %q", key.Value), Warning: true})
			continue
		}
		if want, ok := keyKinds[key.Value]; ok && val.Kind != want && val.Tag != "!!null" {
			out = append(out, Problem{Line: val.Line, Message: fmt.Sprintf("%s must be %s", key.Value, kindName(want))})
			continue
		}
		if boolKeys[key.Value] && val.Tag != "!!bool" {
			out = append(out, Problem{Line: val.Line, Message: key.Value + " must be true or false"})
		}
		switch key.Value {
		case "write_files":
			out = append(out, itemsNeed(val, "write_files", "path")...)
		case "users":
			out = append(out, itemsNeed(val, "users", "name")...)
		}
	}
	return out
}

// itemsNeed reports mapping items of a list that lack field. Plain strings like "default" pass.
func itemsNeed(list *yaml.Node, key, field string) []Problem {
	if list.Kind != yaml.SequenceNode {
		return nil
	}
	var out []Problem
	for _, item := range list.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		found := false
		for j := 0; j+1 < len(item.Content); j += 2 {
			found = found || item.Content[j].Value == field
		}
		if !found {
			out = append(out, Problem{Line: item.Line, Message: fmt.Sprintf("%s entry needs %q", key, field)})
		}
	}
	return out
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	}
	return "a single value"
}

func toSet(words string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(words) {
		set[w] = true
	}
	return set
}
