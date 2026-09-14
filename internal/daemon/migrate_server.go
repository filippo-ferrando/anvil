package daemon

import (
	"context"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/migrate"
)

// MigrateServer implements anvilv1.MigrateServiceServer against a
// *migrate.Manager, streaming its progress over gRPC.
type MigrateServer struct {
	anvilv1.UnimplementedMigrateServiceServer
	Manager *migrate.Manager
}

func NewMigrateServer(mgr *migrate.Manager) *MigrateServer {
	return &MigrateServer{Manager: mgr}
}

func (s *MigrateServer) Migrate(req *anvilv1.MigrateRequest, stream anvilv1.MigrateService_MigrateServer) error {
	params := migrate.Params{
		Name:       req.GetName(),
		To:         req.GetTo(),
		Copy:       req.GetCopy(),
		DestName:   req.GetDestName(),
		DryRun:     req.GetDryRun(),
		BestEffort: req.GetBestEffort(),
	}
	result, err := s.Manager.Migrate(stream.Context(), params, func(status string) {
		_ = stream.Send(&anvilv1.MigrateProgress{Event: &anvilv1.MigrateProgress_Status{Status: status}})
	})
	if err != nil {
		_ = stream.Send(&anvilv1.MigrateProgress{Event: &anvilv1.MigrateProgress_Error{Error: err.Error()}})
		return err
	}

	// An intent migration reports one member_done per member plus a final
	// intent_done summary instead of a single done event.
	if result.IntentName != "" {
		members := make([]*anvilv1.MigrateMemberResult, 0, len(result.Members))
		for _, mr := range result.Members {
			pbResult := &anvilv1.MigrateMemberResult{Role: mr.Role, NewId: mr.NewID}
			if mr.Err != nil {
				pbResult.Error = mr.Err.Error()
			}
			members = append(members, pbResult)
			_ = stream.Send(&anvilv1.MigrateProgress{Event: &anvilv1.MigrateProgress_MemberDone{MemberDone: pbResult}})
		}
		_ = stream.Send(&anvilv1.MigrateProgress{Event: &anvilv1.MigrateProgress_IntentDone{IntentDone: &anvilv1.IntentMigrateDone{
			IntentName: result.IntentName,
			Members:    members,
			RolledBack: result.RolledBack,
		}}})
		return nil
	}

	if result.InstanceID != "" {
		_ = stream.Send(&anvilv1.MigrateProgress{Event: &anvilv1.MigrateProgress_Done{Done: result.InstanceID}})
	}
	return nil
}

func (s *MigrateServer) Key(ctx context.Context, req *anvilv1.MigrateKeyRequest) (*anvilv1.MigrateKeyReply, error) {
	key, err := s.Manager.EnsurePublicKey()
	if err != nil {
		return nil, err
	}
	return &anvilv1.MigrateKeyReply{PublicKey: key}, nil
}

func (s *MigrateServer) GuestKey(ctx context.Context, req *anvilv1.MigrateGuestKeyRequest) (*anvilv1.MigrateGuestKeyReply, error) {
	key, err := s.Manager.GuestKey(ctx, req.GetTo())
	if err != nil {
		return nil, err
	}
	return &anvilv1.MigrateGuestKeyReply{PublicKey: key}, nil
}
