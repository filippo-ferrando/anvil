package instance

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"strings"
)

// maxNameLen matches the 63-character limit in ValidateName.
const maxNameLen = 63

// NameFromImage turns an image ref into a valid instance name, e.g. "ubuntu:24.04"
// gives "ubuntu-24.04" and "docker.io/library/nginx:alpine" gives "nginx-alpine".
func NameFromImage(imageRef string) string {
	ref, _, _ := strings.Cut(imageRef, "@")
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	var b strings.Builder
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
		default:
			if s := b.String(); s != "" && !strings.HasSuffix(s, "-") {
				b.WriteByte('-')
			}
		}
	}
	name := strings.TrimLeft(b.String(), "._-")
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	name = strings.TrimRight(name, "._-")
	if name == "" {
		return "instance"
	}
	return name
}

// UniqueName returns base, or base with a "-2", "-3", ... suffix, whichever taken reports free.
func UniqueName(base string, taken func(string) bool) (string, error) {
	if !taken(base) {
		return base, nil
	}
	for i := 2; i < 1000; i++ {
		suffix := "-" + strconv.Itoa(i)
		name := base
		if len(name)+len(suffix) > maxNameLen {
			name = strings.TrimRight(name[:maxNameLen-len(suffix)], "._-")
		}
		name += suffix
		if !taken(name) {
			return name, nil
		}
	}
	return "", errors.New("instance: no free name left for " + base)
}

// DefaultName picks a free instance name derived from imageRef.
func (m *Manager) DefaultName(imageRef string) (string, error) {
	return UniqueName(NameFromImage(imageRef), func(name string) bool {
		_, err := m.registry.GetByName(name)
		return !errors.Is(err, ErrNotFound)
	})
}

// ReserveDefaultName picks a free name derived from imageRef and holds it until release,
// so two unnamed launches can't pick the same one. Launch with the returned ctx uses it.
func (m *Manager) ReserveDefaultName(ctx context.Context, imageRef string) (context.Context, string, func(), error) {
	inRegistry := func(name string) bool {
		_, err := m.registry.GetByName(name)
		return !errors.Is(err, ErrNotFound)
	}
	var release func()
	name, err := UniqueName(NameFromImage(imageRef), func(name string) bool {
		if inRegistry(name) {
			return true
		}
		r, err := m.names.acquire(name, false)
		if err != nil {
			return true
		}
		// Checked again: a launch may have saved the name and let it go meanwhile.
		if inRegistry(name) {
			r()
			return true
		}
		release = r
		return false
	})
	if err != nil {
		return ctx, "", nil, err
	}
	reserved := maps.Clone(reservedNames(ctx))
	if reserved == nil {
		reserved = map[string]bool{}
	}
	reserved[name] = true
	return context.WithValue(ctx, reservedKey{}, reserved), name, release, nil
}
