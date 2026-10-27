// Package gui holds the fixed paths the daemon and clients agree on for `anvil gui`.
package gui

const (
	// ContainerRuntimeDir is where a container sees its relay directory; used as XDG_RUNTIME_DIR.
	ContainerRuntimeDir = "/run/anvil-gui"
	// ContainerX11Dir is where X11 clients look for display sockets.
	ContainerX11Dir = "/tmp/.X11-unix"
	// XAuthorityName is the cookie file the client writes inside ContainerRuntimeDir.
	XAuthorityName = "Xauthority"

	// GuestToolsDir is where a VM mounts the shipped waypipe and xwayland-satellite.
	GuestToolsDir = "/opt/anvil"
	// ToolsTag is the virtiofs tag of that share.
	ToolsTag = "anviltools"
)
