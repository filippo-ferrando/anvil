package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/container"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
	"github.com/anvil-project/anvil/internal/vm/image"
)

// VMCatalog is the subset of a VM backend ImageServer needs, kept as an
// interface because internal/vm is Linux/QEMU-only and this package must build on every platform.
type VMCatalog interface {
	ListCatalog() ([]image.DistroEntry, error)
}

// ImageServer implements anvilv1.ImageServiceServer, managing the cached VM
// base image store/catalog and the container engines' own cached images.
type ImageServer struct {
	anvilv1.UnimplementedImageServiceServer
	Store      *store.Store
	Vault      *image.Vault
	Backend    VMCatalog
	Containers *container.Backend
}

func NewImageServer(s *store.Store, v *image.Vault, backend VMCatalog, containers *container.Backend) *ImageServer {
	return &ImageServer{Store: s, Vault: v, Backend: backend, Containers: containers}
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

func (s *ImageServer) ListContainerImages(ctx context.Context, req *anvilv1.ContainerImageListRequest) (*anvilv1.ContainerImageListReply, error) {
	images, err := s.Containers.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	reply := &anvilv1.ContainerImageListReply{}
	for _, img := range images {
		reply.Images = append(reply.Images, &anvilv1.ContainerImage{
			Id:        img.ID,
			Engine:    containerEngineToPB(img.Engine),
			RepoTags:  img.RepoTags,
			SizeBytes: img.SizeBytes,
			RefCount:  int32(img.RefCount),
		})
	}
	return reply, nil
}

func (s *ImageServer) DeleteContainerImage(ctx context.Context, req *anvilv1.ContainerImageDeleteRequest) (*anvilv1.ContainerImageDeleteReply, error) {
	engine := containerEngineFromPB(req.GetEngine())
	if err := s.Containers.DeleteImage(ctx, engine, req.GetId(), req.GetForce()); err != nil {
		return nil, err
	}
	return &anvilv1.ContainerImageDeleteReply{}, nil
}

// usersOf returns the names of specs whose disk's backing file is imagePath.
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
// symlinks first and falling back to path comparison.
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
