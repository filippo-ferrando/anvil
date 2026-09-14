package daemon

import (
	"context"
	"fmt"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/store"
	"github.com/anvil-project/anvil/internal/vm/image"
)

// MirrorServer implements anvilv1.MirrorServiceServer against *store.Store.
type MirrorServer struct {
	anvilv1.UnimplementedMirrorServiceServer
	Store *store.Store
}

func NewMirrorServer(s *store.Store) *MirrorServer {
	return &MirrorServer{Store: s}
}

func mirrorKindFromPB(k anvilv1.MirrorKind) store.MirrorKind {
	switch k {
	case anvilv1.MirrorKind_MIRROR_KIND_VM:
		return store.MirrorKindVM
	case anvilv1.MirrorKind_MIRROR_KIND_CONTAINER:
		return store.MirrorKindContainer
	default:
		return ""
	}
}

func mirrorKindToPB(k store.MirrorKind) anvilv1.MirrorKind {
	switch k {
	case store.MirrorKindVM:
		return anvilv1.MirrorKind_MIRROR_KIND_VM
	case store.MirrorKindContainer:
		return anvilv1.MirrorKind_MIRROR_KIND_CONTAINER
	default:
		return anvilv1.MirrorKind_MIRROR_KIND_UNSPECIFIED
	}
}

func mirrorToPB(m store.Mirror) *anvilv1.Mirror {
	return &anvilv1.Mirror{
		Name:        m.Name,
		Kind:        mirrorKindToPB(m.Kind),
		ManifestUrl: m.ManifestURL,
		Registry:    m.Registry,
		MirrorOf:    m.MirrorOf,
		Insecure:    m.Insecure,
		Priority:    int32(m.Priority),
		Enabled:     m.Enabled,
	}
}

func (s *MirrorServer) Add(ctx context.Context, req *anvilv1.MirrorAddRequest) (*anvilv1.MirrorAddReply, error) {
	pb := req.GetMirror()
	if pb == nil {
		return nil, fmt.Errorf("mirror: request must include a mirror")
	}
	if pb.GetName() == "" {
		return nil, fmt.Errorf("mirror: name must not be empty")
	}

	m := store.Mirror{
		Name:        pb.GetName(),
		Kind:        mirrorKindFromPB(pb.GetKind()),
		ManifestURL: pb.GetManifestUrl(),
		Registry:    pb.GetRegistry(),
		MirrorOf:    pb.GetMirrorOf(),
		Insecure:    pb.GetInsecure(),
		Priority:    int(pb.GetPriority()),
		Enabled:     true,
	}

	switch m.Kind {
	case store.MirrorKindVM:
		if m.ManifestURL == "" {
			return nil, fmt.Errorf("mirror: a vm mirror needs a manifest_url")
		}
		raw, err := image.FetchManifest(ctx, m.ManifestURL)
		if err != nil {
			return nil, err
		}
		m.ManifestJSON = string(raw)
	case store.MirrorKindContainer:
		if m.Registry == "" {
			return nil, fmt.Errorf("mirror: a container mirror needs a registry")
		}
	default:
		return nil, fmt.Errorf("mirror: kind must be \"vm\" or \"container\"")
	}

	if err := s.Store.PutMirror(m); err != nil {
		return nil, err
	}
	return &anvilv1.MirrorAddReply{}, nil
}

func (s *MirrorServer) List(ctx context.Context, req *anvilv1.MirrorListRequest) (*anvilv1.MirrorListReply, error) {
	mirrors, err := s.Store.ListMirrors(mirrorKindFromPB(req.GetKindFilter()))
	if err != nil {
		return nil, err
	}
	reply := &anvilv1.MirrorListReply{}
	for _, m := range mirrors {
		reply.Mirrors = append(reply.Mirrors, mirrorToPB(m))
	}
	return reply, nil
}

func (s *MirrorServer) Remove(ctx context.Context, req *anvilv1.MirrorRemoveRequest) (*anvilv1.MirrorRemoveReply, error) {
	if err := s.Store.DeleteMirror(req.GetName()); err != nil {
		return nil, err
	}
	return &anvilv1.MirrorRemoveReply{}, nil
}

func (s *MirrorServer) SetEnabled(ctx context.Context, req *anvilv1.MirrorSetEnabledRequest) (*anvilv1.MirrorSetEnabledReply, error) {
	if err := s.Store.SetMirrorEnabled(req.GetName(), req.GetEnabled()); err != nil {
		return nil, err
	}
	return &anvilv1.MirrorSetEnabledReply{}, nil
}
