//go:build linux

package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGUIToolsScript(t *testing.T) {
	s := guiToolsScript(virtiofsToolsMount)
	for _, want := range []string{
		"mount -t virtiofs -o ro anviltools /opt/anvil",
		"grep -qs ' /opt/anvil ' /proc/mounts",
		"ln -sf /opt/anvil/$f /usr/local/bin/$f",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestXwaylandInstallScriptPerPackageManager(t *testing.T) {
	cases := map[string]string{
		"apt-get": "apt-get install -y -q xwayland",
		"dnf":     "dnf install -y -q xorg-x11-server-Xwayland",
		"zypper":  "zypper --non-interactive -q install xwayland",
		"pacman":  "pacman -Syu --noconfirm --needed xorg-xwayland",
		"apk":     "apk add -q xwayland mesa-egl xkbcomp xkeyboard-config",
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for manager, want := range cases {
		t.Run(manager, func(t *testing.T) {
			// A fake PATH with only this package manager, which logs what it was asked to do.
			bin := t.TempDir()
			log := filepath.Join(t.TempDir(), "log")
			for _, tool := range []string{"sh", "cat", "grep", "basename", "chmod"} {
				if p, err := exec.LookPath(tool); err == nil {
					_ = os.Symlink(p, filepath.Join(bin, tool))
				}
			}
			fake := "#!" + sh + "\necho \"$(basename \"$0\") $*\" >> " + log + "\n"
			if manager == "apt-get" {
				fake += "[ \"$1\" = install ] && printf '#!" + sh + "\\n' > " + bin + "/Xwayland && chmod +x " + bin + "/Xwayland\n"
			} else {
				fake += "printf '#!" + sh + "\\n' > " + bin + "/Xwayland && chmod +x " + bin + "/Xwayland\n"
			}
			if err := os.WriteFile(filepath.Join(bin, manager), []byte(fake+"exit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			script := strings.Replace(xwaylandInstallScript(), guestPath, "export PATH="+bin, 1)
			out, err := exec.Command(sh, "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			got, _ := os.ReadFile(log)
			if !strings.Contains(string(got), strings.TrimPrefix(want, manager+" ")) {
				t.Fatalf("log %q, want %q", got, want)
			}
		})
	}
}

func TestXwaylandInstallScriptSkipsWhenPresent(t *testing.T) {
	bin := t.TempDir()
	sh, _ := exec.LookPath("sh")
	_ = os.Symlink(sh, filepath.Join(bin, "sh"))
	_ = os.WriteFile(filepath.Join(bin, "Xwayland"), []byte("#!"+sh+"\n"), 0o755)
	_ = os.WriteFile(filepath.Join(bin, "apt-get"), []byte("#!"+sh+"\nexit 9\n"), 0o755)
	script := strings.Replace(xwaylandInstallScript(), guestPath, "export PATH="+bin, 1)
	if out, err := exec.Command(sh, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("should exit early: %v %s", err, out)
	}
}
