package hostdev

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneLink(t *testing.T) {
	host, peer, err := CloneLink("10.253.0.4/30")
	if err != nil || host.String() != "10.253.0.5/30" || peer.String() != "10.253.0.6/30" {
		t.Fatalf("CloneLink = %s, %s, %v", host, peer, err)
	}
	for _, bad := range []string{"", "10.253.0.5/30", "10.253.0.0/29", "8.8.8.0/30", "fd00::/126", "nope"} {
		if _, _, err := CloneLink(bad); err == nil {
			t.Errorf("CloneLink(%q) accepted", bad)
		}
	}
}

func TestNetnsValidate(t *testing.T) {
	if err := (Netns{ID: "eph-01JX9ZQ", Link: "10.253.0.0/30"}).validate(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []Netns{
		{ID: "01JX9ZQ", Link: "10.253.0.0/30"},     // not a clone
		{ID: "eph-../x", Link: "10.253.0.0/30"},    // path escape
		{ID: "eph-a b", Link: "10.253.0.0/30"},     // space
		{ID: "eph-01JX9ZQ", Link: "10.200.0.0/30"}, // the in-netns layout
		{ID: "eph-01JX9ZQ", Link: "192.168.1.1/30"},
	} {
		if err := n.validate(); err == nil {
			t.Errorf("validate(%+v) accepted", n)
		}
	}
	if rep := handle(request{Op: "netns-create"}, os.Getuid(), os.Getgid()); !strings.Contains(rep.Error, "without a netns") {
		t.Errorf("netns-create without a netns = %+v", rep)
	}
	if rep := handle(request{Op: "netns-delete", Netns: &Netns{ID: "../etc"}}, os.Getuid(), os.Getgid()); rep.Error == "" {
		t.Error("netns-delete accepted a bad id")
	}
}

func TestCloneVeth(t *testing.T) {
	a, b := CloneVeth("eph-01JX9ZQ"), CloneVeth("eph-01JX9ZR")
	if len(a) > 15 || !strings.HasPrefix(a, "anve") || a == b || a != CloneVeth("eph-01JX9ZQ") {
		t.Errorf("CloneVeth = %q, %q", a, b)
	}
}

// TestNetnsOverTheSocket sends the netns requests through Client to handle, as the
// helper's socket does, and checks the reply's error comes back.
func TestNetnsOverTheSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "helper.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan request, 2)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			var req request
			if json.NewDecoder(conn).Decode(&req) == nil {
				got <- req
				_ = json.NewEncoder(conn).Encode(handle(req, os.Getuid(), os.Getgid()))
			}
			conn.Close()
		}
	}()
	c := Client{Socket: sock}
	ctx := context.Background()
	if err := c.CreateNetns(ctx, Netns{ID: "eph-X", Link: "8.8.8.0/30"}); err == nil || !strings.Contains(err.Error(), "private IPv4 /30") {
		t.Errorf("CreateNetns with a public link = %v", err)
	}
	if err := c.DeleteNetns(ctx, "not-a-clone"); err == nil || !strings.Contains(err.Error(), "invalid clone id") {
		t.Errorf("DeleteNetns with a bad id = %v", err)
	}
	first, second := <-got, <-got
	if first.Op != "netns-create" || first.Netns.Link != "8.8.8.0/30" || second.Op != "netns-delete" {
		t.Errorf("requests = %+v, %+v", first, second)
	}
}
