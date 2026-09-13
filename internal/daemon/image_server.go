package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
	"github.com/anvil-project/anvil/internal/vm"
	"github.com/anvil-project/anvil/internal/vm/image"
)

// ImageServer implements anvilv1.ImageServiceServer: the on-disk cache of
// downloaded VM base images (internal/vm/image.Vault's "prepared" tier,
// List/Delete) plus the source catalog of what can be downloaded and
// launched in the first place (Catalog, via *vm.Backend so it's exactly
// the same built-in-plus-mirrors merge a real launch resolves against,
// not a separate copy of that logic). Before deleting a cached image, it
// checks every current VM instance's disk (via image.BackingFile) so it
// never silently deletes an image a running instance's overlay still
// depends on — that would corrupt that instance's disk, since a qcow2
// overlay needs its backing file to stay put.
type ImageServer struct {
	anvilv1.UnimplementedImageServiceServer
	Store   *store.Store
	Vault   *image.Vault
	Backend *vm.Backend
}

func NewImageServer(s *store.Store, v *image.Vault, backend *vm.Backend) *ImageServer {
	return &ImageServer{Store: s, Vault: v, Backend: backend}
}

func (s *ImageServer) Catalog(ctx context.Context, req *anvilv1.CatalogRequest) (*anvilv1.CatalogReply, error) {
	entries, err := s.Backend.ListCatalog()
	if err != nil {
		return nil, err
	}
	reply := &anvilv1.CatalogReply{}
	for _, e := range entries {
		reply.Entries = append(reply.Entries, &anvilv1.CatalogEntry{
			Id:          e.ID,
			Name:        e.Name,
			Distro:      e.Distro,
			Version:     e.Version,
			Arch:        e.Arch,
			MinDiskGib:  e.MinDiskGiB,
			DefaultUser: e.DefaultUser,
		})
	}
	return reply, nil
}

func (s *ImageServer) List(ctx context.Context, req *anvilv1.ImageListRequest) (*anvilv1.ImageListReply, error) {
	cached, err := s.Vault.List()
	if err != nil {
		return nil, err
	}
	specs, err := s.Store.List(instance.KindVM)
	if err != nil {
		return nil, err
	}

	reply := &anvilv1.ImageListReply{}
	for _, img := range cached {
		users := usersOf(img.Path, specs)
		reply.Images = append(reply.Images, &anvilv1.CachedImage{
			Id:        img.ID,
			Arch:      img.Arch,
			Path:      img.Path,
			SizeBytes: img.SizeBytes,
			RefCount:  int32(len(users)),
		})
	}
	return reply, nil
}

func (s *ImageServer) Delete(ctx context.Context, req *anvilv1.ImageDeleteRequest) (*anvilv1.ImageDeleteReply, error) {
	cached, err := s.Vault.List()
	if err != nil {
		return nil, err
	}

	var target *image.CachedImage
	for i := range cached {
		if cached[i].ID != req.GetId() {
			continue
		}
		if req.GetArch() != "" && cached[i].Arch != req.GetArch() {
			continue
		}
		target = &cached[i]
		break
	}
	if target == nil {
		return nil, fmt.Errorf("image: no cached image matching id %q", req.GetId())
	}

	if !req.GetForce() {
		specs, err := s.Store.List(instance.KindVM)
		if err != nil {
			return nil, err
		}
		if users := usersOf(target.Path, specs); len(users) > 0 {
			return nil, fmt.Errorf("image: %q is still used by instance(s) %s; pass --force to delete anyway (this will break their disks)",
				req.GetId(), strings.Join(users, ", "))
		}
	}

	if err := s.Vault.Delete(target.Path); err != nil {
		return nil, err
	}
	return &anvilv1.ImageDeleteReply{}, nil
}

// usersOf returns the names of specs that currently have imagePath as
// their disk's backing file. Best-effort: a spec whose disk can't be
// inspected (already deleted, mid-transition, whatever) is silently
// skipped rather than blocking the whole check — it's not this image's
// problem.
//
// Compares via samePath, not a plain string equality: imagePath is
// computed by joining config.PreparedImageDir() with a filename, while
// backing is whatever qemu-img itself reports for the overlay's
// -b argument — normally identical, but if any directory on that path
// (StateDir, CacheDir, or something above them) is a symlink, the two
// strings can refer to the same file without being byte-identical, which
// a plain == would silently and permanently report as "not in use."
func usersOf(imagePath string, specs []*instance.Spec) (names []string) {
	for _, spec := range specs {
		if spec.VM == nil || spec.VM.DiskPath == "" {
			continue
		}
		backing, err := image.BackingFile(spec.VM.DiskPath)
		if err != nil || backing == "" {
			continue
		}
		if samePath(backing, imagePath) {
			names = append(names, spec.Name)
		}
	}
	return names
}

// samePath reports whether a and b refer to the same file, resolving
// symlinks first (falling back to filepath.Clean, and then plain string
// equality, if either side can't be resolved — e.g. because a path was
// never actually valid, which a plain comparison still catches).
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}
