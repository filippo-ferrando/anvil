package daemon

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// The golden strings are what the TUI wizard rendered before generation moved behind the daemon.
func TestRenderCloudConfigGolden(t *testing.T) {
	tests := []struct {
		name string
		req  *anvilv1.CloudInitGenerateRequest
		want string
	}{
		{"empty", &anvilv1.CloudInitGenerateRequest{}, "#cloud-config\n"},
		{
			"all fields, blanks dropped",
			&anvilv1.CloudInitGenerateRequest{
				User:        "ubuntu",
				SshKeys:     []string{"ssh-ed25519 AAAAC3Nza user@host"},
				Packages:    []string{"nginx", " curl", "", "git "},
				RunCommands: []string{"systemctl enable nginx", " ufw allow 80, 443", " "},
			},
			"#cloud-config\npackages:\n    - nginx\n    - curl\n    - git\nruncmd:\n    - systemctl enable nginx\n    - ufw allow 80, 443\nusers:\n    - name: ubuntu\n      ssh-authorized-keys:\n        - ssh-ed25519 AAAAC3Nza user@host\n      sudo: ALL=(ALL) NOPASSWD:ALL\n",
		},
		{
			"yaml-significant characters stay literal",
			&anvilv1.CloudInitGenerateRequest{User: `ubuntu: "evil"`, SshKeys: []string{"ssh-ed25519 AAAA... - not a list item"}},
			"#cloud-config\nusers:\n    - name: 'ubuntu: \"evil\"'\n      ssh-authorized-keys:\n        - ssh-ed25519 AAAA... - not a list item\n      sudo: ALL=(ALL) NOPASSWD:ALL\n",
		},
		{"packages only", &anvilv1.CloudInitGenerateRequest{Packages: []string{"nginx"}}, "#cloud-config\npackages:\n    - nginx\n"},
		{
			"user without keys",
			&anvilv1.CloudInitGenerateRequest{User: "admin", RunCommands: []string{"echo hi"}},
			"#cloud-config\nruncmd:\n    - echo hi\nusers:\n    - name: admin\n      sudo: ALL=(ALL) NOPASSWD:ALL\n",
		},
		{
			"several keys",
			&anvilv1.CloudInitGenerateRequest{User: "admin", SshKeys: []string{"ssh-ed25519 A", "ssh-rsa B"}},
			"#cloud-config\nusers:\n    - name: admin\n      ssh-authorized-keys:\n        - ssh-ed25519 A\n        - ssh-rsa B\n      sudo: ALL=(ALL) NOPASSWD:ALL\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderCloudConfig(tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestCloudInitGenerate(t *testing.T) {
	ci := NewCloudInitServer(testStore(t))

	t.Run("keys without a user are rejected", func(t *testing.T) {
		_, err := ci.Generate(t.Context(), &anvilv1.CloudInitGenerateRequest{SshKeys: []string{"ssh-ed25519 A"}})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("err = %v, want InvalidArgument", err)
		}
	})

	t.Run("no save_as only renders", func(t *testing.T) {
		if _, err := ci.Generate(t.Context(), &anvilv1.CloudInitGenerateRequest{User: "a"}); err != nil {
			t.Fatal(err)
		}
		list, err := ci.List(t.Context(), &anvilv1.CloudInitListRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if n := len(list.GetConfigs()); n != 0 {
			t.Fatalf("library has %d configs, want 0", n)
		}
	})

	t.Run("save_as stores the rendered content", func(t *testing.T) {
		reply, err := ci.Generate(t.Context(), &anvilv1.CloudInitGenerateRequest{User: "a", SaveAs: "web"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := ci.Get(t.Context(), &anvilv1.CloudInitGetRequest{Name: "web"})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetContent() != reply.GetContent() {
			t.Fatalf("saved %q, rendered %q", got.GetContent(), reply.GetContent())
		}
	})
}
