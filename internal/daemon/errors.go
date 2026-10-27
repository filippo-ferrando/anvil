package daemon

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// wrapErr converts instance.ErrNotFound to codes.NotFound and instance.ErrNoNames
// to codes.InvalidArgument, passing any other error through unchanged.
func wrapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, instance.ErrNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}
	if errors.Is(err, instance.ErrNoNames) {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return err
}
