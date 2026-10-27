package commands

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

type fakeHostClient struct {
	anvilv1.HostServiceClient
	added []*anvilv1.Host
}

func (f *fakeHostClient) Add(_ context.Context, req *anvilv1.HostAddRequest, _ ...grpc.CallOption) (*anvilv1.HostAddReply, error) {
	f.added = append(f.added, req.GetHost())
	return &anvilv1.HostAddReply{}, nil
}

func TestAddDiscoveredHost(t *testing.T) {
	hosts := []*anvilv1.DiscoveredHost{
		{Name: "forge", Address: "192.168.1.9", SshPort: 2222},
		{Name: "nas", Address: "192.168.1.10", SshPort: 22},
	}

	t.Run("defaults match the TUI pre-fill", func(t *testing.T) {
		fc := &fakeHostClient{}
		if _, err := addDiscoveredHost(t.Context(), fc, hosts, discoverAddOptions{key: "forge", user: "root"}); err != nil {
			t.Fatal(err)
		}
		if len(fc.added) != 1 || fc.added[0].GetAlias() != "forge" || fc.added[0].GetTarget() != "root@192.168.1.9:2222" {
			t.Fatalf("added %v", fc.added)
		}
	})

	t.Run("alias, user and key options are passed on", func(t *testing.T) {
		fc := &fakeHostClient{}
		o := discoverAddOptions{key: "192.168.1.10", alias: "store", user: "admin", identity: "/k/id", strictKey: true}
		if _, err := addDiscoveredHost(t.Context(), fc, hosts, o); err != nil {
			t.Fatal(err)
		}
		h := fc.added[0]
		if h.GetAlias() != "store" || h.GetTarget() != "admin@192.168.1.10" || h.GetIdentity() != "/k/id" || !h.GetStrictHostKey() {
			t.Fatalf("added %v", h)
		}
	})

	t.Run("an unknown name adds nothing", func(t *testing.T) {
		fc := &fakeHostClient{}
		if _, err := addDiscoveredHost(t.Context(), fc, hosts, discoverAddOptions{key: "nope", user: "root"}); err == nil {
			t.Fatal("expected an error")
		}
		if len(fc.added) != 0 {
			t.Fatalf("added %v", fc.added)
		}
	})
}

func TestHostDiscoverAddOnlyFlagsNeedAdd(t *testing.T) {
	cmd := newHostDiscoverCommand(&globalFlags{})
	cmd.SetArgs([]string{"--user", "admin"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--add") {
		t.Fatalf("expected a --add error before dialing, got %v", err)
	}
}
