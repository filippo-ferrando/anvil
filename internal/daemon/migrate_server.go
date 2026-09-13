package daemon

import (
	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/migrate"
)

// MigrateServer implements anvilv1.MigrateServiceServer against a
// *migrate.Manager — all the actual SSH/export/relaunch logic lives
// there, this just streams its progress callback out over gRPC, same
// pattern as Server.Launch.
type MigrateServer struct {
	anvilv1.UnimplementedMigrateServiceServer
	Manager *migrate.Manager
}

func NewMigrateServer(mgr *migrate.Manager) *MigrateServer {
	return &MigrateServer{Manager: mgr}
}

func (s *MigrateServer) Migrate(req *anvilv1.MigrateRequest, stream anvilv1.MigrateService_MigrateServer) error {
	params := migrate.Params{
		Name:     req.GetName(),
		To:       req.GetTo(),
		Copy:     req.GetCopy(),
		DestName: req.GetDestName(),
		DryRun:   req.GetDryRun(),
	}
	newID, err := s.Manager.Migrate(stream.Context(), params, func(status string) {
		_ = stream.Send(&anvilv1.MigrateProgress{Event: &anvilv1.MigrateProgress_Status{Status: status}})
	})
	if err != nil {
		_ = stream.Send(&anvilv1.MigrateProgress{Event: &anvilv1.MigrateProgress_Error{Error: err.Error()}})
		return err
	}
	if newID != "" {
		_ = stream.Send(&anvilv1.MigrateProgress{Event: &anvilv1.MigrateProgress_Done{Done: newID}})
	}
	return nil
}
