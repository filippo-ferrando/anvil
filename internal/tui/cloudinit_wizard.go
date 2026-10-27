package tui

import (
	"strings"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// newWizardRequest maps the wizard fields to a Generate request; the daemon trims entries and drops blank ones.
// Run commands split on ";" since shell commands often contain commas.
func newWizardRequest(name, user, sshKey, packagesCSV, runCmdsSemi string) *anvilv1.CloudInitGenerateRequest {
	req := &anvilv1.CloudInitGenerateRequest{
		SaveAs:      name,
		User:        user,
		Packages:    strings.Split(packagesCSV, ","),
		RunCommands: strings.Split(runCmdsSemi, ";"),
	}
	if sshKey != "" {
		req.SshKeys = []string{sshKey}
	}
	return req
}
