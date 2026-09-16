package tui

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// buildWizardCloudInit renders a #cloud-config document for the cloud-init
// page's "w" quick-start wizard: an optional default user (with an optional
// SSH key and passwordless sudo), a package list, and a runcmd list. Values
// are run through yaml.Marshal rather than string-concatenated, so a colon,
// quote, or dash typed into a name/key/package/command can't corrupt the
// document. packagesCSV is comma-separated (package names never contain a
// comma); runCmdsSemi is semicolon-separated instead, since a shell command
// commonly contains commas of its own (e.g. install flags/arguments).
func buildWizardCloudInit(user, sshKey, packagesCSV, runCmdsSemi string) (string, error) {
	doc := map[string]any{}

	if user != "" {
		u := map[string]any{
			"name": user,
			"sudo": "ALL=(ALL) NOPASSWD:ALL",
		}
		if sshKey != "" {
			u["ssh-authorized-keys"] = []string{sshKey}
		}
		doc["users"] = []any{u}
	}

	if packages := splitNonEmpty(packagesCSV, ","); len(packages) > 0 {
		doc["packages"] = packages
	}
	if runCmds := splitNonEmpty(runCmdsSemi, ";"); len(runCmds) > 0 {
		doc["runcmd"] = runCmds
	}

	if len(doc) == 0 {
		return "#cloud-config\n", nil
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + string(data), nil
}

// splitNonEmpty splits s on sep, trims each piece, and drops empty ones —
// so a trailing separator or stray extra whitespace doesn't produce a
// blank list entry.
func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
