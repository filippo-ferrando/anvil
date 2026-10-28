package cloudinit

import "testing"

func TestValidate(t *testing.T) {
	cases := []struct {
		name, data string
		line       int // of the first problem, 0 = expect none
		warning    bool
	}{
		{"valid", "#cloud-config\npackages: [nginx]\nwrite_files:\n  - path: /etc/x\n    content: hi\nusers: [default, {name: bob}]\n", 0, false},
		{"script", "#!/bin/sh\necho hi\n", 0, false},
		{"empty config", "#cloud-config\n", 0, false},
		{"no header", "packages: [nginx]\n", 1, false},
		{"bad yaml", "#cloud-config\npackages: [nginx\nruncmd: []\n", 2, false},
		{"not a mapping", "#cloud-config\n- a\n", 2, false},
		{"unknown key", "#cloud-config\npackage: [nginx]\n", 2, true},
		{"wrong kind", "#cloud-config\nruncmd: reboot\n", 2, false},
		{"not a bool", "#cloud-config\npackage_update: yes please\n", 2, false},
		{"file without path", "#cloud-config\nwrite_files:\n  - content: hi\n", 3, false},
		{"user without name", "#cloud-config\nusers:\n  - groups: sudo\n", 3, false},
	}
	for _, tc := range cases {
		ps := Validate(tc.data)
		if tc.line == 0 {
			if len(ps) != 0 {
				t.Errorf("%s: unexpected problems %+v", tc.name, ps)
			}
			continue
		}
		if len(ps) == 0 || ps[0].Line != tc.line || ps[0].Warning != tc.warning {
			t.Errorf("%s: got %+v, want line %d warning %v", tc.name, ps, tc.line, tc.warning)
		}
	}
}
