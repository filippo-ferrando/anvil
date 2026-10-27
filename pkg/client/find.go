package client

import (
	"strings"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// MatchesFindTerm reports whether a catalog entry's id, distro, version or
// name contains term, ignoring case. An empty term matches everything.
func MatchesFindTerm(e *anvilv1.CatalogEntry, term string) bool {
	term = strings.ToLower(term)
	return strings.Contains(strings.ToLower(e.GetId()), term) ||
		strings.Contains(strings.ToLower(e.GetDistro()), term) ||
		strings.Contains(strings.ToLower(e.GetVersion()), term) ||
		strings.Contains(strings.ToLower(e.GetName()), term)
}
