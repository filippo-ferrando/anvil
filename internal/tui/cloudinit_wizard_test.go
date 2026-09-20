package tui

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBuildWizardCloudInit(t *testing.T) {
	t.Run("empty inputs produce a bare cloud-config", func(t *testing.T) {
		content, err := buildWizardCloudInit("", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if content != "#cloud-config\n" {
			t.Fatalf("got %q", content)
		}
	})

	t.Run("packages field tolerates blank entries and whitespace", func(t *testing.T) {
		content, err := buildWizardCloudInit("", "", "nginx, , curl ,", "")
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Packages []string `yaml:"packages"`
		}
		if err := yaml.Unmarshal([]byte(strings.TrimPrefix(content, "#cloud-config\n")), &doc); err != nil {
			t.Fatal(err)
		}
		if want := []string{"nginx", "curl"}; !equalStrings(doc.Packages, want) {
			t.Fatalf("packages = %v, want %v", doc.Packages, want)
		}
	})

	// runcmd is semicolon-separated (not comma-separated, like packages)
	// because a shell command commonly contains commas of its own.
	t.Run("run commands are split on semicolons, commas pass through untouched", func(t *testing.T) {
		content, err := buildWizardCloudInit("", "", "", "apt-get install -y a, b; ufw allow 80 ;")
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			RunCmd []string `yaml:"runcmd"`
		}
		if err := yaml.Unmarshal([]byte(strings.TrimPrefix(content, "#cloud-config\n")), &doc); err != nil {
			t.Fatal(err)
		}
		want := []string{"apt-get install -y a, b", "ufw allow 80"}
		if !equalStrings(doc.RunCmd, want) {
			t.Fatalf("runcmd = %v, want %v", doc.RunCmd, want)
		}
	})

	// A value containing YAML-significant characters (colon, quotes, a leading dash) must round-trip as literal
	// text, not corrupt the document; this was the bug in the original hand-concatenated string-building version.
	t.Run("special characters in values don't corrupt the document", func(t *testing.T) {
		user := `ubuntu: "evil"`
		key := "ssh-ed25519 AAAA... - not a list item"
		content, err := buildWizardCloudInit(user, key, "", "")
		if err != nil {
			t.Fatal(err)
		}

		var doc struct {
			Users []struct {
				Name              string   `yaml:"name"`
				Sudo              string   `yaml:"sudo"`
				SSHAuthorizedKeys []string `yaml:"ssh-authorized-keys"`
			} `yaml:"users"`
		}
		if err := yaml.Unmarshal([]byte(strings.TrimPrefix(content, "#cloud-config\n")), &doc); err != nil {
			t.Fatalf("generated document isn't valid YAML: %v\n%s", err, content)
		}
		if len(doc.Users) != 1 {
			t.Fatalf("users = %v", doc.Users)
		}
		if doc.Users[0].Name != user {
			t.Fatalf("name = %q, want %q", doc.Users[0].Name, user)
		}
		if len(doc.Users[0].SSHAuthorizedKeys) != 1 || doc.Users[0].SSHAuthorizedKeys[0] != key {
			t.Fatalf("ssh-authorized-keys = %v, want [%q]", doc.Users[0].SSHAuthorizedKeys, key)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
