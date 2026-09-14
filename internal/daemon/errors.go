package daemon

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/anvil-project/anvil/internal/instance"
)

// wrapErr converts instance.ErrNotFound into a gRPC codes.NotFound status,
// passing any other error through unchanged.
func wrapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, instance.ErrNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}
	return err
}
