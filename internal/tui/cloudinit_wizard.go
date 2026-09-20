package tui

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// buildWizardCloudInit renders a #cloud-config document for the wizard's quick-start fields, using yaml.Marshal
// (not string concatenation) so values can't corrupt the document. packagesCSV is comma-separated; runCmdsSemi is semicolon-separated since shell commands often contain commas of their own.
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

// splitNonEmpty splits s on sep, trims each piece, and drops empty entries,
// so a trailing separator or stray whitespace doesn't produce a blank list entry.
func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
