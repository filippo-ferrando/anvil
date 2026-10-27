package daemon

import (
	"context"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

func TestDefaultLaunchName(t *testing.T) {
	s := newTestServer(t, readyBackend{})
	taken := instance.LaunchParams{Name: "ubuntu-24.04", Kind: instance.KindVM, VM: &instance.VMSpec{}, NoStart: true}
	if err := s.Manager.Launch(context.Background(), taken, func(instance.LaunchEvent) {}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		params instance.LaunchParams
		want   string
	}{
		{instance.LaunchParams{VM: &instance.VMSpec{ImageRef: "ubuntu:24.04"}}, "ubuntu-24.04-2"},
		{instance.LaunchParams{Container: &instance.ContainerSpec{ImageRef: "docker.io/library/nginx:alpine"}}, "nginx-alpine"},
		{instance.LaunchParams{IntentName: "shop", Role: "db", VM: &instance.VMSpec{ImageRef: "debian-13"}}, "shop-db"},
	}
	for _, c := range cases {
		p := c.params
		if err := s.defaultLaunchName(&p); err != nil || p.Name != c.want {
			t.Errorf("got %q, %v; want %q", p.Name, err, c.want)
		}
	}
}
