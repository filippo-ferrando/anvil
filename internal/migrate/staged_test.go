package migrate

import "testing"

func TestIsStagedDataPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/var/tmp/anvil-migrate-data-01H-0":      true,
		"/var/tmp/anvil-migrate-data-01H-0/":     true,
		"/tmp/anvil-migrate-data-01H-0":          false,
		"/home/u/anvil-migrate-data-x":           false,
		"/var/tmp/x/../anvil-migrate-data-01H-0": true,
		"/var/tmp/sub/anvil-migrate-data-01H-0":  false,
		"/var/tmp/other":                         false,
	} {
		if got := IsStagedDataPath(p); got != want {
			t.Errorf("IsStagedDataPath(%q) = %v, want %v", p, got, want)
		}
	}
}
