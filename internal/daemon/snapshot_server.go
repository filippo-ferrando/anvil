package daemon

import (
	"context"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/instance"
)

// SnapshotServer implements anvilv1.SnapshotServiceServer against an
// *instance.Manager.
type SnapshotServer struct {
	anvilv1.UnimplementedSnapshotServiceServer
	Manager *instance.Manager
}

func NewSnapshotServer(mgr *instance.Manager) *SnapshotServer {
	return &SnapshotServer{Manager: mgr}
}

func (s *SnapshotServer) Create(ctx context.Context, req *anvilv1.SnapshotCreateRequest) (*anvilv1.SnapshotCreateReply, error) {
	if err := s.Manager.CreateSnapshot(ctx, req.GetName(), req.GetSnapshotName()); err != nil {
		return nil, err
	}
	return &anvilv1.SnapshotCreateReply{}, nil
}

func (s *SnapshotServer) Restore(ctx context.Context, req *anvilv1.SnapshotRestoreRequest) (*anvilv1.SnapshotRestoreReply, error) {
	if err := s.Manager.RestoreSnapshot(ctx, req.GetName(), req.GetSnapshotName()); err != nil {
		return nil, err
	}
	return &anvilv1.SnapshotRestoreReply{}, nil
}

func (s *SnapshotServer) Delete(ctx context.Context, req *anvilv1.SnapshotDeleteRequest) (*anvilv1.SnapshotDeleteReply, error) {
	if err := s.Manager.DeleteSnapshot(ctx, req.GetName(), req.GetSnapshotName()); err != nil {
		return nil, err
	}
	return &anvilv1.SnapshotDeleteReply{}, nil
}

func (s *SnapshotServer) List(ctx context.Context, req *anvilv1.SnapshotListRequest) (*anvilv1.SnapshotListReply, error) {
	snaps, err := s.Manager.ListSnapshots(ctx, req.GetName())
	if err != nil {
		return nil, err
	}
	reply := &anvilv1.SnapshotListReply{}
	for _, snap := range snaps {
		reply.Snapshots = append(reply.Snapshots, &anvilv1.SnapshotInfo{
			Name:          snap.Name,
			CreatedAtUnix: snap.CreatedAt.Unix(),
			HasVmState:    snap.HasVMState,
		})
	}
	return reply, nil
}
