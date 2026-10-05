package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/anvil-project/anvil/internal/instance"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestRecordsRoundTrip covers put, get-by-id, get-by-name, list and delete for
// every record family the registry holds.
func TestRecordsRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		put  func(*Store) error
		// get reads the record back by its primary key.
		get func(*Store) (any, error)
		// getAlt reads it by its secondary key, where one exists.
		getAlt func(*Store) (any, error)
		count  func(*Store) (int, error)
		del    func(*Store) error
		want   any
	}{
		{
			name: "instance",
			put: func(s *Store) error {
				return s.PutInstance(&instance.Spec{ID: "01", Name: "web", Kind: instance.KindVM, State: instance.StateRunning})
			},
			get: func(s *Store) (any, error) {
				sp, err := s.GetByID("01")
				if err != nil {
					return nil, err
				}
				return sp.Name, nil
			},
			getAlt: func(s *Store) (any, error) {
				sp, err := s.GetByName("web")
				if err != nil {
					return nil, err
				}
				return sp.ID, nil
			},
			count: func(s *Store) (int, error) { l, err := s.List(""); return len(l), err },
			del:   func(s *Store) error { return s.DeleteByID("01") },
			want:  "web",
		},
		{
			name: "intent",
			put: func(s *Store) error {
				return s.PutIntent(Intent{ID: "i1", Name: "myapp", Members: []IntentMember{{InstanceID: "01", Role: "web"}}})
			},
			get:    func(s *Store) (any, error) { it, err := s.GetIntentByID("i1"); return it.Name, err },
			getAlt: func(s *Store) (any, error) { it, err := s.GetIntentByName("myapp"); return it.ID, err },
			count:  func(s *Store) (int, error) { l, err := s.ListIntents(); return len(l), err },
			del:    func(s *Store) error { return s.DeleteIntentByID("i1") },
			want:   "myapp",
		},
		{
			name: "host",
			put: func(s *Store) error {
				return s.PutHost(Host{Alias: "box", Target: "root@10.0.0.2", StrictHostKey: true})
			},
			get: func(s *Store) (any, error) { h, err := s.GetHost("box"); return h.Target, err },
			count: func(s *Store) (int, error) {
				l, err := s.ListHosts()
				return len(l), err
			},
			del:  func(s *Store) error { return s.DeleteHost("box") },
			want: "root@10.0.0.2",
		},
		{
			name: "mirror",
			put: func(s *Store) error {
				return s.PutMirror(Mirror{Name: "corp", Kind: MirrorKindVM, ManifestURL: "https://example.org/m.json", Enabled: true})
			},
			get:   func(s *Store) (any, error) { m, err := s.GetMirror("corp"); return m.ManifestURL, err },
			count: func(s *Store) (int, error) { l, err := s.ListMirrors(""); return len(l), err },
			del:   func(s *Store) error { return s.DeleteMirror("corp") },
			want:  "https://example.org/m.json",
		},
		{
			name:  "cloud-init",
			put:   func(s *Store) error { return s.SaveCloudInit("base", "#cloud-config\n{}\n") },
			get:   func(s *Store) (any, error) { c, err := s.GetCloudInit("base"); return c.Content, err },
			count: func(s *Store) (int, error) { l, err := s.ListCloudInit(); return len(l), err },
			del:   func(s *Store) error { return s.DeleteCloudInit("base") },
			want:  "#cloud-config\n{}\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)

			if n, err := tc.count(s); err != nil || n != 0 {
				t.Fatalf("a fresh store listed %d %s(s) (err %v), want 0", n, tc.name, err)
			}
			if err := tc.put(s); err != nil {
				t.Fatalf("put: %v", err)
			}
			got, err := tc.get(s)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got != tc.want {
				t.Errorf("get returned %v, want %v", got, tc.want)
			}
			if tc.getAlt != nil {
				if _, err := tc.getAlt(s); err != nil {
					t.Errorf("lookup by secondary key: %v", err)
				}
			}
			if n, err := tc.count(s); err != nil || n != 1 {
				t.Errorf("list returned %d (err %v), want 1", n, err)
			}

			if err := tc.del(s); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, err := tc.get(s); !errors.Is(err, instance.ErrNotFound) {
				t.Errorf("get after delete returned %v, want ErrNotFound", err)
			}
			if n, err := tc.count(s); err != nil || n != 0 {
				t.Errorf("list after delete returned %d (err %v), want 0", n, err)
			}
		})
	}
}

// TestRecordsSurviveReopen makes sure the records are really on disk and not
// just in a cache, since the daemon reads them back after a restart.
func TestRecordsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anvil.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutInstance(&instance.Spec{ID: "01", Name: "web", Kind: instance.KindVM}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutHost(Host{Alias: "box", Target: "root@10.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	if spec, err := reopened.GetByName("web"); err != nil || spec.ID != "01" {
		t.Errorf("instance after reopen: %+v, %v", spec, err)
	}
	if h, err := reopened.GetHost("box"); err != nil || h.Target != "root@10.0.0.2" {
		t.Errorf("host after reopen: %+v, %v", h, err)
	}
}

func TestListFiltersByKind(t *testing.T) {
	s := openTestStore(t)
	for _, spec := range []*instance.Spec{
		{ID: "01", Name: "vm-a", Kind: instance.KindVM},
		{ID: "02", Name: "vm-b", Kind: instance.KindVM},
		{ID: "03", Name: "ctr-a", Kind: instance.KindContainer},
	} {
		if err := s.PutInstance(spec); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		kind instance.Kind
		want int
	}{
		{kind: "", want: 3},
		{kind: instance.KindVM, want: 2},
		{kind: instance.KindContainer, want: 1},
	}
	for _, tc := range tests {
		got, err := s.List(tc.kind)
		if err != nil {
			t.Fatalf("List(%q): %v", tc.kind, err)
		}
		if len(got) != tc.want {
			t.Errorf("List(%q) returned %d, want %d", tc.kind, len(got), tc.want)
		}
	}
}

func TestListMirrorsFiltersByKind(t *testing.T) {
	s := openTestStore(t)
	for _, m := range []Mirror{
		{Name: "vm-one", Kind: MirrorKindVM},
		{Name: "ctr-one", Kind: MirrorKindContainer},
		{Name: "ctr-two", Kind: MirrorKindContainer},
	} {
		if err := s.PutMirror(m); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		kind MirrorKind
		want int
	}{
		{kind: "", want: 3},
		{kind: MirrorKindVM, want: 1},
		{kind: MirrorKindContainer, want: 2},
	}
	for _, tc := range tests {
		got, err := s.ListMirrors(tc.kind)
		if err != nil {
			t.Fatalf("ListMirrors(%q): %v", tc.kind, err)
		}
		if len(got) != tc.want {
			t.Errorf("ListMirrors(%q) returned %d, want %d", tc.kind, len(got), tc.want)
		}
	}
}

func TestRenameAndToggleHelpers(t *testing.T) {
	s := openTestStore(t)
	if err := s.SaveCloudInit("old", "#cloud-config\n{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameCloudInit("old", "new"); err != nil {
		t.Fatalf("RenameCloudInit: %v", err)
	}
	if _, err := s.GetCloudInit("old"); !errors.Is(err, instance.ErrNotFound) {
		t.Errorf("the old name should be gone, got %v", err)
	}
	if c, err := s.GetCloudInit("new"); err != nil || c.Content == "" {
		t.Errorf("the renamed template lost its content: %+v, %v", c, err)
	}

	if err := s.PutMirror(Mirror{Name: "corp", Kind: MirrorKindVM, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMirrorEnabled("corp", false); err != nil {
		t.Fatalf("SetMirrorEnabled: %v", err)
	}
	if m, err := s.GetMirror("corp"); err != nil || m.Enabled {
		t.Errorf("mirror should be disabled: %+v, %v", m, err)
	}
}
