package daemon

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

func TestWrapErrCodes(t *testing.T) {
	cases := []struct {
		err  error
		want codes.Code
	}{
		{fmt.Errorf("x: %w", instance.ErrNotFound), codes.NotFound},
		{instance.ErrNoNames, codes.InvalidArgument},
		{fmt.Errorf("instance: name %q is %w", "web", instance.ErrNameInUse), codes.AlreadyExists},
		{fmt.Errorf("instance: web: %w", instance.ErrBusy), codes.Aborted},
		{errors.New("disk on fire"), codes.Unknown},
	}
	for _, c := range cases {
		if got := status.Code(wrapErr(c.err)); got != c.want {
			t.Errorf("%v: got %v, want %v", c.err, got, c.want)
		}
	}
}
