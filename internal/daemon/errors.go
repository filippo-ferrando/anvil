package daemon

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// wrapErr maps instance errors to gRPC codes (NotFound, InvalidArgument, AlreadyExists,
// Aborted) and passes any other error through unchanged.
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
	if errors.Is(err, instance.ErrNameInUse) {
		return status.Error(codes.AlreadyExists, err.Error())
	}
	if errors.Is(err, instance.ErrBusy) {
		return status.Error(codes.Aborted, err.Error())
	}
	return err
}
