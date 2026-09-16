package daemon

import (
	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/export"
)

// ExportServer implements anvilv1.ExportServiceServer against an
// *export.Manager, streaming its progress over gRPC.
type ExportServer struct {
	anvilv1.UnimplementedExportServiceServer
	Manager *export.Manager
}

func NewExportServer(mgr *export.Manager) *ExportServer {
	return &ExportServer{Manager: mgr}
}

func (s *ExportServer) Export(req *anvilv1.ExportRequest, stream anvilv1.ExportService_ExportServer) error {
	params := export.ExportParams{Name: req.GetName(), OutputPath: req.GetOutputPath(), IsIntent: req.GetIsIntent()}
	err := s.Manager.Export(stream.Context(), params, func(status string) {
		_ = stream.Send(&anvilv1.ExportProgress{Event: &anvilv1.ExportProgress_Status{Status: status}})
	})
	if err != nil {
		_ = stream.Send(&anvilv1.ExportProgress{Event: &anvilv1.ExportProgress_Error{Error: err.Error()}})
		return err
	}
	_ = stream.Send(&anvilv1.ExportProgress{Event: &anvilv1.ExportProgress_Done{Done: req.GetOutputPath()}})
	return nil
}

func (s *ExportServer) Import(req *anvilv1.ImportRequest, stream anvilv1.ExportService_ImportServer) error {
	params := export.ImportParams{BundlePath: req.GetBundlePath(), Name: req.GetName()}
	result, err := s.Manager.Import(stream.Context(), params, func(status string) {
		_ = stream.Send(&anvilv1.ImportProgress{Event: &anvilv1.ImportProgress_Status{Status: status}})
	})
	if err != nil {
		_ = stream.Send(&anvilv1.ImportProgress{Event: &anvilv1.ImportProgress_Error{Error: err.Error()}})
		return err
	}

	members := make([]*anvilv1.ImportMemberResult, 0, len(result.Members))
	for _, mr := range result.Members {
		members = append(members, &anvilv1.ImportMemberResult{Role: mr.Role, NewId: mr.NewID})
	}
	_ = stream.Send(&anvilv1.ImportProgress{Event: &anvilv1.ImportProgress_Done{Done: &anvilv1.ImportDone{
		IntentName: result.IntentName,
		Members:    members,
	}}})
	return nil
}
