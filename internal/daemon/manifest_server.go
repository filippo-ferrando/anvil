package daemon

import (
	"context"

	"gopkg.in/yaml.v3"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// ManifestServer implements anvilv1.ManifestServiceServer against *store.Store.
type ManifestServer struct {
	anvilv1.UnimplementedManifestServiceServer
	Store *store.Store
}

func NewManifestServer(s *store.Store) *ManifestServer {
	return &ManifestServer{Store: s}
}

// manifestInfo reads only the intent: field, so a draft that doesn't fully validate still lists.
func manifestInfo(d store.Doc) *anvilv1.ManifestInfo {
	var head struct {
		Intent string `yaml:"intent"`
	}
	_ = yaml.Unmarshal([]byte(d.Content), &head)
	return &anvilv1.ManifestInfo{Name: d.Name, ModifiedAtUnix: d.ModifiedAt.Unix(), Intent: head.Intent}
}

func (s *ManifestServer) List(ctx context.Context, req *anvilv1.ManifestListRequest) (*anvilv1.ManifestListReply, error) {
	docs, err := s.Store.ListManifests()
	if err != nil {
		return nil, err
	}
	reply := &anvilv1.ManifestListReply{}
	for _, d := range docs {
		reply.Manifests = append(reply.Manifests, manifestInfo(d))
	}
	return reply, nil
}

func (s *ManifestServer) Get(ctx context.Context, req *anvilv1.ManifestGetRequest) (*anvilv1.ManifestGetReply, error) {
	d, err := s.Store.GetManifest(req.GetName())
	if err != nil {
		return nil, wrapErr(err)
	}
	return &anvilv1.ManifestGetReply{Info: manifestInfo(d), Content: d.Content}, nil
}

func (s *ManifestServer) Save(ctx context.Context, req *anvilv1.ManifestSaveRequest) (*anvilv1.ManifestSaveReply, error) {
	if err := s.Store.SaveManifest(req.GetName(), req.GetContent()); err != nil {
		return nil, err
	}
	return &anvilv1.ManifestSaveReply{}, nil
}

func (s *ManifestServer) Rename(ctx context.Context, req *anvilv1.ManifestRenameRequest) (*anvilv1.ManifestRenameReply, error) {
	if err := s.Store.RenameManifest(req.GetOldName(), req.GetNewName()); err != nil {
		return nil, wrapErr(err)
	}
	return &anvilv1.ManifestRenameReply{}, nil
}

func (s *ManifestServer) Delete(ctx context.Context, req *anvilv1.ManifestDeleteRequest) (*anvilv1.ManifestDeleteReply, error) {
	if err := s.Store.DeleteManifest(req.GetName()); err != nil {
		return nil, wrapErr(err)
	}
	return &anvilv1.ManifestDeleteReply{}, nil
}
