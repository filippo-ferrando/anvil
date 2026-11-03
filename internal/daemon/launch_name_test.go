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
		_, release, err := s.defaultLaunchName(context.Background(), &p)
		if err != nil || p.Name != c.want {
			t.Errorf("got %q, %v; want %q", p.Name, err, c.want)
		}
		if release != nil {
			release()
		}
	}

	// Two unnamed launches at once get different names while both are held.
	a := instance.LaunchParams{VM: &instance.VMSpec{ImageRef: "debian:13"}}
	b := a
	ctxA, releaseA, err := s.defaultLaunchName(context.Background(), &a)
	if err != nil {
		t.Fatal(err)
	}
	_, releaseB, err := s.defaultLaunchName(context.Background(), &b)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseB()
	if a.Name == b.Name {
		t.Fatalf("both launches got %q", a.Name)
	}
	// The holder of a reservation can still launch with it.
	a.Kind, a.NoStart = instance.KindVM, true
	if err := s.Manager.Launch(ctxA, a, func(instance.LaunchEvent) {}); err != nil {
		t.Fatalf("launch with the reserved name: %v", err)
	}
	releaseA()
}
