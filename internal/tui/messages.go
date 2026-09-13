package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/pkg/client"
)

// Every RPC call becomes a tea.Cmd (a func() tea.Msg run by the bubbletea
// runtime in its own goroutine) that reports back one of these messages —
// the same "daemon owns all logic, this is just a thin client" mandate as
// the CLI, just wired through bubbletea's message loop instead of a
// direct function call.

type instancesLoadedMsg struct {
	instances []*anvilv1.Instance
	err       error
}

type actionDoneMsg struct {
	verb string // "started" / "stopped" / "deleted" — for the status line
	err  error
}

type cachedImagesLoadedMsg struct {
	images []*anvilv1.CachedImage
	err    error
}

type catalogLoadedMsg struct {
	entries []*anvilv1.CatalogEntry
	err     error
}

type cloudInitListLoadedMsg struct {
	configs []*anvilv1.CloudInitConfigInfo
	err     error
}

type cloudInitContentLoadedMsg struct {
	name, content string
	err           error
}

type cloudInitSavedMsg struct {
	name string
	err  error
}

type mirrorsLoadedMsg struct {
	mirrors []*anvilv1.Mirror
	err     error
}

type hostsLoadedMsg struct {
	hosts []*anvilv1.Host
	err   error
}

type hostTestedMsg struct {
	alias, detail string
	ok            bool
	err           error
}

// launchStreamMsg/migrateStreamMsg carry one event off a streaming RPC,
// plus the stream itself so the handler can chain a "receive the next
// one" command — the standard Bubble Tea pattern for consuming a
// server-streaming gRPC call one message at a time without blocking the
// UI loop. done is set on the terminal event (success or failure), at
// which point stream is nil and there's nothing left to chain.
type launchStreamMsg struct {
	stream   anvilv1.InstanceService_LaunchClient
	status   string
	instance *anvilv1.Instance
	err      error
	done     bool
}

type migrateStreamMsg struct {
	stream anvilv1.MigrateService_MigrateClient
	line   string
	err    error
	done   bool
}

func loadInstances(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.List(context.Background(), &anvilv1.ListRequest{})
		if err != nil {
			return instancesLoadedMsg{err: err}
		}
		return instancesLoadedMsg{instances: reply.GetInstances()}
	}
}

func loadCachedImages(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Image.List(context.Background(), &anvilv1.ImageListRequest{})
		if err != nil {
			return cachedImagesLoadedMsg{err: err}
		}
		return cachedImagesLoadedMsg{images: reply.GetImages()}
	}
}

func loadCatalog(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Image.Catalog(context.Background(), &anvilv1.CatalogRequest{})
		if err != nil {
			return catalogLoadedMsg{err: err}
		}
		return catalogLoadedMsg{entries: reply.GetEntries()}
	}
}

func deleteCachedImage(c *client.Client, id string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Image.Delete(context.Background(), &anvilv1.ImageDeleteRequest{Id: id})
		return actionDoneMsg{verb: "deleted", err: err}
	}
}

func startInstance(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Start(context.Background(), &anvilv1.StartRequest{Names: []string{name}})
		return actionDoneMsg{verb: "started", err: err}
	}
}

func stopInstance(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Stop(context.Background(), &anvilv1.StopRequest{Names: []string{name}})
		return actionDoneMsg{verb: "stopped", err: err}
	}
}

func deleteInstance(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Delete(context.Background(), &anvilv1.DeleteRequest{Names: []string{name}})
		return actionDoneMsg{verb: "deleted", err: err}
	}
}

func loadCloudInitList(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.CloudInit.List(context.Background(), &anvilv1.CloudInitListRequest{})
		if err != nil {
			return cloudInitListLoadedMsg{err: err}
		}
		return cloudInitListLoadedMsg{configs: reply.GetConfigs()}
	}
}

func loadCloudInitContent(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.CloudInit.Get(context.Background(), &anvilv1.CloudInitGetRequest{Name: name})
		if err != nil {
			return cloudInitContentLoadedMsg{name: name, err: err}
		}
		return cloudInitContentLoadedMsg{name: name, content: reply.GetContent()}
	}
}

func saveCloudInit(c *client.Client, name, content string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.CloudInit.Save(context.Background(), &anvilv1.CloudInitSaveRequest{Name: name, Content: content})
		return cloudInitSavedMsg{name: name, err: err}
	}
}

func renameCloudInit(c *client.Client, oldName, newName string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.CloudInit.Rename(context.Background(), &anvilv1.CloudInitRenameRequest{OldName: oldName, NewName: newName})
		return cloudInitSavedMsg{name: newName, err: err}
	}
}

func deleteCloudInit(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.CloudInit.Delete(context.Background(), &anvilv1.CloudInitDeleteRequest{Name: name})
		return actionDoneMsg{verb: "deleted", err: err}
	}
}

func loadMirrors(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Mirror.List(context.Background(), &anvilv1.MirrorListRequest{})
		if err != nil {
			return mirrorsLoadedMsg{err: err}
		}
		return mirrorsLoadedMsg{mirrors: reply.GetMirrors()}
	}
}

func addMirror(c *client.Client, m *anvilv1.Mirror) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Mirror.Add(context.Background(), &anvilv1.MirrorAddRequest{Mirror: m})
		return actionDoneMsg{verb: "added", err: err}
	}
}

func setMirrorEnabled(c *client.Client, name string, enabled bool) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Mirror.SetEnabled(context.Background(), &anvilv1.MirrorSetEnabledRequest{Name: name, Enabled: enabled})
		return actionDoneMsg{verb: "updated", err: err}
	}
}

func removeMirror(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Mirror.Remove(context.Background(), &anvilv1.MirrorRemoveRequest{Name: name})
		return actionDoneMsg{verb: "removed", err: err}
	}
}

func loadHosts(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Host.List(context.Background(), &anvilv1.HostListRequest{})
		if err != nil {
			return hostsLoadedMsg{err: err}
		}
		return hostsLoadedMsg{hosts: reply.GetHosts()}
	}
}

func addHost(c *client.Client, h *anvilv1.Host) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Host.Add(context.Background(), &anvilv1.HostAddRequest{Host: h})
		return actionDoneMsg{verb: "added", err: err}
	}
}

func removeHost(c *client.Client, alias string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Host.Remove(context.Background(), &anvilv1.HostRemoveRequest{Alias: alias})
		return actionDoneMsg{verb: "removed", err: err}
	}
}

func testHost(c *client.Client, alias string) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Host.Test(context.Background(), &anvilv1.HostTestRequest{Alias: alias})
		if err != nil {
			return hostTestedMsg{alias: alias, err: err}
		}
		return hostTestedMsg{alias: alias, ok: reply.GetOk(), detail: reply.GetMessage()}
	}
}
