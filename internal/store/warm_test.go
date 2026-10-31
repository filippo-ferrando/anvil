package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

func TestWarmRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	for _, w := range []WarmSnapshot{
		{Key: "bbb", Distro: "ubuntu-24.04", CPUs: 2, MemoryMiB: 1024, CreatedAt: now},
		{Key: "aaa", Distro: "ubuntu-24.04", CPUs: 2, MemoryMiB: 512, SizeBytes: 42},
		{Key: "ccc", Distro: "alpine-3.22", CPUs: 1, MemoryMiB: 512},
	} {
		if err := s.PutWarm(w); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutWarm(WarmSnapshot{}); err == nil {
		t.Error("PutWarm accepted an empty key")
	}
	list, err := s.ListWarm()
	if err != nil || len(list) != 3 || list[0].Key != "ccc" || list[1].Key != "aaa" || list[2].Key != "bbb" {
		t.Fatalf("ListWarm = %+v, %v", list, err)
	}
	if got, err := s.GetWarm("bbb"); err != nil || !got.CreatedAt.Equal(now) {
		t.Errorf("GetWarm = %+v, %v", got, err)
	}
	if err := s.DeleteWarm("aaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWarm("aaa"); !errors.Is(err, instance.ErrNotFound) {
		t.Errorf("GetWarm after delete = %v", err)
	}
	if err := s.DeleteWarm("aaa"); !errors.Is(err, instance.ErrNotFound) {
		t.Errorf("second DeleteWarm = %v", err)
	}
}

func TestWarmPoolRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := WarmPool{Distro: "ubuntu-24.04", Arch: "x86_64", CPUs: 2, MemoryMiB: 512, Size: 2}
	b := WarmPool{Distro: "alpine-3.22", Arch: "x86_64", CPUs: 1, MemoryMiB: 256, Size: 4}
	for _, p := range []WarmPool{a, b} {
		if err := s.PutWarmPool(p); err != nil {
			t.Fatal(err)
		}
	}
	a.Size = 3 // same distro and size: replaces the first
	if err := s.PutWarmPool(a); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListWarmPools()
	if err != nil || len(got) != 2 || got[0] != b || got[1] != a {
		t.Fatalf("ListWarmPools = %+v, %v", got, err)
	}
	if err := s.DeleteWarmPool(a.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWarmPool(a.ID()); err != nil {
		t.Errorf("deleting a missing pool: %v", err)
	}
	if got, _ := s.ListWarmPools(); len(got) != 1 || got[0] != b {
		t.Errorf("after delete: %+v", got)
	}
}
