package instance

import "testing"

func TestNormalizeArch(t *testing.T) {
	for in, want := range map[string]string{"amd64": "x86_64", "arm64": "aarch64", " x86_64\n": "x86_64", "riscv64": "riscv64"} {
		if got := NormalizeArch(in); got != want {
			t.Errorf("NormalizeArch(%q) = %q, want %q", in, got, want)
		}
	}
	if HostArch() == "amd64" || HostArch() == "arm64" {
		t.Errorf("HostArch() = %q, want VM naming", HostArch())
	}
}
