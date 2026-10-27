package client

import (
	"fmt"
	"strings"
	"time"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/vars"
	"git.fferrando.cc/rdfilippo/anvil/internal/sshkey"
)

// ApplyOptions are the `anvil apply` settings besides the manifest itself.
type ApplyOptions struct {
	DryRun, Prune, Recreate bool
	Vars                    []string // KEY=VALUE overrides
	EnvFile                 string   // empty: .env beside the manifest, if present
	WaitTimeout             time.Duration
}

// NewApplyRequest resolves the manifest's variables on the client side, since
// the daemon can't see the caller's env or files. path locates a default .env.
func NewApplyRequest(path string, data []byte, opts ApplyOptions) (*anvilv1.IntentApplyRequest, error) {
	overrides, err := ParseVars(opts.Vars)
	if err != nil {
		return nil, err
	}
	values, err := vars.Load(path, opts.EnvFile, data, overrides)
	if err != nil {
		return nil, err
	}
	pub, err := sshkey.EnsureDefaultPublic()
	if err != nil {
		return nil, err
	}
	return &anvilv1.IntentApplyRequest{
		Manifest:           data,
		DryRun:             opts.DryRun,
		Prune:              opts.Prune,
		Recreate:           opts.Recreate,
		SshPublicKeys:      []string{pub},
		WaitTimeoutSeconds: CeilSeconds(opts.WaitTimeout),
		Vars:               values,
	}, nil
}

// ParseVars parses KEY=VALUE manifest variable overrides.
func ParseVars(kvs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("var %q: expected KEY=VALUE", kv)
		}
		out[k] = v
	}
	return out, nil
}

// CeilSeconds rounds d up to whole seconds, so a sub-second timeout isn't sent as 0 (the default).
func CeilSeconds(d time.Duration) int32 {
	if d <= 0 {
		return 0
	}
	return int32((d + time.Second - 1) / time.Second)
}
