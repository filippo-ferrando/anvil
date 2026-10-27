package daemon

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// TestEmptyNamesInvalidArgument checks that Start/Stop/Delete with no names fail
// with InvalidArgument once the interceptor has wrapped the error.
func TestEmptyNamesInvalidArgument(t *testing.T) {
	s := newTestServer(t, readyBackend{})
	ctx := context.Background()
	_, startErr := s.Start(ctx, &anvilv1.StartRequest{})
	_, stopErr := s.Stop(ctx, &anvilv1.StopRequest{})
	_, delErr := s.Delete(ctx, &anvilv1.DeleteRequest{})
	for name, err := range map[string]error{"start": startErr, "stop": stopErr, "delete": delErr} {
		if got := status.Code(wrapErr(err)); got != codes.InvalidArgument {
			t.Errorf("%s: got code %v (%v), want InvalidArgument", name, got, err)
		}
	}
}
