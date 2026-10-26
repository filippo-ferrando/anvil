//go:build !linux

package hostdev

import "errors"

// Serve is Linux only: there is no USB/PCI passthrough elsewhere.
func Serve() error { return errors.New("devhelper: only supported on Linux") }
