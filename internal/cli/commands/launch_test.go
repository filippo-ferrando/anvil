package commands

import (
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestLaunchOptionsValidate(t *testing.T) {
	tests := []struct {
		name    string
		opts    launchOptions
		command []string
		want    anvilv1.Kind
		wantErr string
	}{
		{name: "vm", opts: launchOptions{kind: "vm"}, want: anvilv1.Kind_KIND_VM},
		{name: "container with command", opts: launchOptions{kind: "container"}, command: []string{"sh"}, want: anvilv1.Kind_KIND_CONTAINER},
		{name: "missing kind", opts: launchOptions{}, wantErr: "--kind must be"},
		{name: "vm with command", opts: launchOptions{kind: "vm"}, command: []string{"sh"}, wantErr: "trailing command"},
		{name: "ephemeral-only flag", opts: launchOptions{kind: "vm", eph: ephemeralFlags{tty: true}}, wantErr: "only apply with --ephemeral"},
		{name: "role without intent", opts: launchOptions{kind: "vm", role: "db"}, wantErr: "--role only makes sense"},
		{name: "firecracker device", opts: launchOptions{kind: "vm", hypervisor: "firecracker", devices: []string{"usb:1-1"}}, wantErr: "--device only applies"},
		{name: "container hypervisor", opts: launchOptions{kind: "container", hypervisor: "qemu"}, wantErr: "--hypervisor only applies"},
		{name: "wait and no-start", opts: launchOptions{kind: "vm", wait: true, noStart: true}, wantErr: "mutually exclusive"},
		{name: "two cloud-init sources", opts: launchOptions{kind: "vm", cloudInitFile: "a", cloudInitName: "b"}, wantErr: "mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.opts.validate(tt.command)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validate() error = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("validate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLaunchOptionsRequestContainer(t *testing.T) {
	o := launchOptions{kind: "container", entrypoint: "/bin/sh", envVars: []string{"A=1"}, restart: "always"}
	req, err := o.request("alpine", []string{"-c", "true"})
	if err != nil {
		t.Fatal(err)
	}
	c := req.GetContainer()
	if c.GetImageRef() != "alpine" || len(c.GetEntrypoint()) != 1 || len(c.GetCmd()) != 2 {
		t.Errorf("unexpected container spec: %v", c)
	}
	if req.GetVm() != nil {
		t.Error("container request carries a VM spec")
	}
	if req.GetRestartPolicy().GetMode() != "always" {
		t.Errorf("restart policy = %q, want always", req.GetRestartPolicy().GetMode())
	}
}
