package commands

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestIntentRejectsUnknownHypervisor(t *testing.T) {
	for _, newCmd := range []func(*globalFlags) *cobra.Command{newIntentCreateCommand, newIntentAddCommand} {
		cmd := newCmd(&globalFlags{})
		cmd.SetArgs([]string{"lab", "--vm", "web:ubuntu:24.04", "--hypervisor", "xen"})
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "hypervisor") {
			t.Errorf("%s: expected a hypervisor error before dialing, got %v", cmd.Name(), err)
		}
	}
}
