package daemon

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/cloudinitrepo"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// CloudInitServer implements anvilv1.CloudInitServiceServer against *store.Store.
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

// ImportRepo fetches manifestURL and saves each listed template into the
// library, streaming one result per template.
func (s *CloudInitServer) ImportRepo(req *anvilv1.CloudInitImportRepoRequest, stream anvilv1.CloudInitService_ImportRepoServer) error {
	send := func(ev *anvilv1.CloudInitImportRepoProgress) { _ = stream.Send(ev) }

	manifest, err := cloudinitrepo.FetchManifest(stream.Context(), req.GetManifestUrl())
	if err != nil {
		send(&anvilv1.CloudInitImportRepoProgress{Event: &anvilv1.CloudInitImportRepoProgress_Error{Error: err.Error()}})
		return err
	}

	for _, t := range manifest.Templates {
		if !req.GetForce() {
			if _, err := s.Store.GetCloudInit(t.Name); err == nil {
				send(&anvilv1.CloudInitImportRepoProgress{Event: &anvilv1.CloudInitImportRepoProgress_Imported{
					Imported: &anvilv1.CloudInitImportResult{Name: t.Name, Skipped: true},
				}})
				continue
			}
		}

		send(&anvilv1.CloudInitImportRepoProgress{Event: &anvilv1.CloudInitImportRepoProgress_Status{Status: "fetching " + t.Name}})
		content, err := cloudinitrepo.FetchTemplate(stream.Context(), t.URL)
		if err != nil {
			send(&anvilv1.CloudInitImportRepoProgress{Event: &anvilv1.CloudInitImportRepoProgress_Imported{
				Imported: &anvilv1.CloudInitImportResult{Name: t.Name, Error: err.Error()},
			}})
			continue
		}
		if err := s.Store.SaveCloudInit(t.Name, content); err != nil {
			send(&anvilv1.CloudInitImportRepoProgress{Event: &anvilv1.CloudInitImportRepoProgress_Imported{
				Imported: &anvilv1.CloudInitImportResult{Name: t.Name, Error: err.Error()},
			}})
			continue
		}
		send(&anvilv1.CloudInitImportRepoProgress{Event: &anvilv1.CloudInitImportRepoProgress_Imported{
			Imported: &anvilv1.CloudInitImportResult{Name: t.Name},
		}})
	}
	return nil
}

// Generate renders a quick-start #cloud-config and saves it under save_as when set.
func (s *CloudInitServer) Generate(ctx context.Context, req *anvilv1.CloudInitGenerateRequest) (*anvilv1.CloudInitGenerateReply, error) {
	content, err := renderCloudConfig(req)
	if err != nil {
		return nil, err
	}
	if name := req.GetSaveAs(); name != "" {
		if err := s.Store.SaveCloudInit(name, content); err != nil {
			return nil, err
		}
	}
	return &anvilv1.CloudInitGenerateReply{Content: content}, nil
}

// renderCloudConfig uses yaml.Marshal, not string building, so a value can't corrupt the document.
// Entries are trimmed and blank ones dropped.
func renderCloudConfig(req *anvilv1.CloudInitGenerateRequest) (string, error) {
	user := strings.TrimSpace(req.GetUser())
	keys := trimNonEmpty(req.GetSshKeys())
	if user == "" && len(keys) > 0 {
		return "", status.Error(codes.InvalidArgument, "cloud-init: SSH keys need a user to attach to")
	}

	doc := map[string]any{}
	if user != "" {
		u := map[string]any{"name": user, "sudo": "ALL=(ALL) NOPASSWD:ALL"}
		if len(keys) > 0 {
			u["ssh-authorized-keys"] = keys
		}
		doc["users"] = []any{u}
	}
	if packages := trimNonEmpty(req.GetPackages()); len(packages) > 0 {
		doc["packages"] = packages
	}
	if runCmds := trimNonEmpty(req.GetRunCommands()); len(runCmds) > 0 {
		doc["runcmd"] = runCmds
	}

	if len(doc) == 0 {
		return "#cloud-config\n", nil
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + string(data), nil
}

func trimNonEmpty(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
