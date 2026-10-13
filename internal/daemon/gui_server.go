package daemon

import (
	"context"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func (s *Server) PrepareGUI(req *anvilv1.PrepareGUIRequest, stream anvilv1.InstanceService_PrepareGUIServer) error {
	progress := func(status string) {
		_ = stream.Send(&anvilv1.PrepareGUIProgress{Event: &anvilv1.PrepareGUIProgress_Status{Status: status}})
	}
	spec, session, err := s.Manager.PrepareGUI(stream.Context(), req.GetName(), req.GetX11(), progress)
	if err != nil {
		// Reported in-band, like a failed launch.
		return stream.Send(&anvilv1.PrepareGUIProgress{Event: &anvilv1.PrepareGUIProgress_Error{Error: err.Error()}})
	}
	return stream.Send(&anvilv1.PrepareGUIProgress{Event: &anvilv1.PrepareGUIProgress_Session{Session: &anvilv1.GUISession{
		Instance:      specToPB(spec),
		RelayDir:      session.RelayDir,
		X11Dir:        session.X11Dir,
		GuestToolsDir: session.GuestToolsDir,
	}}})
}

func (s *Server) GetGUIAppInfo(ctx context.Context, req *anvilv1.GUIAppInfoRequest) (*anvilv1.GUIAppInfoReply, error) {
	app, found, err := s.Manager.GUIAppInfo(ctx, req.GetName(), req.GetCommand())
	if err != nil {
		return nil, wrapErr(err)
	}
	return &anvilv1.GUIAppInfoReply{
		Found: found, AppName: app.Name, Icon: app.Icon, IconExt: app.IconExt, IconSize: int32(app.IconSize),
	}, nil
}
