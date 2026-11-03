//go:build !linux

package commands

import (
	"context"
	"errors"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

var errGUILinuxOnly = errors.New("anvil gui only works on a Linux Wayland desktop")

func checkLocalDisplay(bool) error { return errGUILinuxOnly }

func runContainerGUI(context.Context, *anvilv1.Instance, *anvilv1.GUISession, guiOptions, guiOutput, []string) error {
	return errGUILinuxOnly
}

func detachGUI(guiOutput, string) error { return errGUILinuxOnly }
