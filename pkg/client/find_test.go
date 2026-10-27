package client

import (
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestMatchesFindTerm(t *testing.T) {
	e := &anvilv1.CatalogEntry{Id: "ubuntu-24.04", Distro: "Ubuntu", Version: "24.04", Name: "Noble Numbat"}
	for term, want := range map[string]bool{
		"":       true,
		"ubuntu": true,
		"UBUNTU": true,
		"24.04":  true,
		"noble":  true,
		"debian": false,
	} {
		if got := MatchesFindTerm(e, term); got != want {
			t.Errorf("MatchesFindTerm(%q) = %v, want %v", term, got, want)
		}
	}
}
