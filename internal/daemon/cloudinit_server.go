package daemon

import (
	"context"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/store"
)

// CloudInitServer implements anvilv1.CloudInitServiceServer directly
// against *store.Store — there's no separate domain/business-logic layer
// here (unlike InstanceService's Manager) since the saved cloud-init
// library is just CRUD, nothing to orchestrate.
type CloudInitServer struct {
	anvilv1.UnimplementedCloudInitServiceServer
	Store *store.Store
}

func NewCloudInitServer(s *store.Store) *CloudInitServer {
	return &CloudInitServer{Store: s}
}

func (s *CloudInitServer) List(ctx context.Context, req *anvilv1.CloudInitListRequest) (*anvilv1.CloudInitListReply, error) {
	configs, err := s.Store.ListCloudInit()
	if err != nil {
		return nil, err
	}
	reply := &anvilv1.CloudInitListReply{}
	for _, c := range configs {
		reply.Configs = append(reply.Configs, &anvilv1.CloudInitConfigInfo{
			Name:           c.Name,
			ModifiedAtUnix: c.ModifiedAt.Unix(),
		})
	}
	return reply, nil
}

func (s *CloudInitServer) Get(ctx context.Context, req *anvilv1.CloudInitGetRequest) (*anvilv1.CloudInitGetReply, error) {
	cfg, err := s.Store.GetCloudInit(req.GetName())
	if err != nil {
		return nil, err
	}
	return &anvilv1.CloudInitGetReply{
		Name:           cfg.Name,
		Content:        cfg.Content,
		ModifiedAtUnix: cfg.ModifiedAt.Unix(),
	}, nil
}

func (s *CloudInitServer) Save(ctx context.Context, req *anvilv1.CloudInitSaveRequest) (*anvilv1.CloudInitSaveReply, error) {
	if err := s.Store.SaveCloudInit(req.GetName(), req.GetContent()); err != nil {
		return nil, err
	}
	return &anvilv1.CloudInitSaveReply{}, nil
}

func (s *CloudInitServer) Rename(ctx context.Context, req *anvilv1.CloudInitRenameRequest) (*anvilv1.CloudInitRenameReply, error) {
	if err := s.Store.RenameCloudInit(req.GetOldName(), req.GetNewName()); err != nil {
		return nil, err
	}
	return &anvilv1.CloudInitRenameReply{}, nil
}

func (s *CloudInitServer) Delete(ctx context.Context, req *anvilv1.CloudInitDeleteRequest) (*anvilv1.CloudInitDeleteReply, error) {
	if err := s.Store.DeleteCloudInit(req.GetName()); err != nil {
		return nil, err
	}
	return &anvilv1.CloudInitDeleteReply{}, nil
}
