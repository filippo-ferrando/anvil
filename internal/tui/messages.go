package tui

import (
	"context"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/pkg/client"
)

// Every RPC call becomes a tea.Cmd that reports back one of these messages.

type instancesLoadedMsg struct {
	instances []*anvilv1.Instance
	err       error
}

// statsLoadedMsg carries one instance's live resource-usage snapshot back
// from a Stats RPC call — name lets the handler ignore a stale reply that
// arrives after the user has since selected a different instance.
type statsLoadedMsg struct {
	name  string
	stats *anvilv1.InstanceStats
	err   error
}

type actionDoneMsg struct {
	verb string // "started" / "stopped" / "deleted" — for the status line

	// screen is which screen's action produced this message, regardless of
	// which screen the user is currently viewing.
	screen screen

	err error
}

type intentsLoadedMsg struct {
	intents []*anvilv1.Intent
	err     error
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

// launchStreamMsg carries one event off the Launch streaming RPC, plus
// the stream itself to chain the next receive. done marks the terminal event.
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

type logsStreamMsg struct {
	stream anvilv1.InstanceService_LogsClient
	data   []byte
	err    error
	done   bool
}

// cloudInitImportStreamMsg carries one event off a CloudInitService.ImportRepo stream.
type cloudInitImportStreamMsg struct {
	stream anvilv1.CloudInitService_ImportRepoClient
	status string
	result *anvilv1.CloudInitImportResult
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

// loadStats fetches name's live resource-usage snapshot. The RPC itself
// takes ~200ms (the daemon samples twice to compute a rate), which is fine
// for a periodic poll but would visibly stutter the UI if called inline.
func loadStats(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Stats(context.Background(), &anvilv1.StatsRequest{Name: name})
		if err != nil {
			return statsLoadedMsg{name: name, err: err}
		}
		return statsLoadedMsg{name: name, stats: reply.GetStats()}
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
		return actionDoneMsg{screen: screenImages, verb: "deleted", err: err}
	}
}

func startInstance(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Start(context.Background(), &anvilv1.StartRequest{Names: []string{name}})
		return actionDoneMsg{screen: screenInstances, verb: "started", err: err}
	}
}

func stopInstance(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Stop(context.Background(), &anvilv1.StopRequest{Names: []string{name}})
		return actionDoneMsg{screen: screenInstances, verb: "stopped", err: err}
	}
}

// deleteInstance deletes an instance; purge also removes it outright rather than leaving it recoverable.
func deleteInstance(c *client.Client, name string, purge bool) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Delete(context.Background(), &anvilv1.DeleteRequest{Names: []string{name}, Purge: purge})
		verb := "deleted"
		if purge {
			verb = "deleted permanently"
		}
		return actionDoneMsg{screen: screenInstances, verb: verb, err: err}
	}
}

func mountInstance(c *client.Client, name, hostPath, guestPath string, readOnly bool) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Mount(context.Background(), &anvilv1.MountRequest{
			Name: name, HostPath: hostPath, GuestPath: guestPath, ReadOnly: readOnly,
		})
		return actionDoneMsg{screen: screenInstances, verb: "mounted", err: err}
	}
}

func umountInstance(c *client.Client, name, guestPath string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Umount(context.Background(), &anvilv1.UmountRequest{Name: name, GuestPath: guestPath})
		return actionDoneMsg{screen: screenInstances, verb: "unmounted", err: err}
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
		return actionDoneMsg{screen: screenCloudInit, verb: "deleted", err: err}
	}
}

func startCloudInitImportRepo(c *client.Client, manifestURL string, force bool) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.CloudInit.ImportRepo(context.Background(), &anvilv1.CloudInitImportRepoRequest{
			ManifestUrl: manifestURL, Force: force,
		})
		if err != nil {
			return cloudInitImportStreamMsg{err: err, done: true}
		}
		return receiveCloudInitImportEvent(stream)()
	}
}

func receiveCloudInitImportEvent(stream anvilv1.CloudInitService_ImportRepoClient) tea.Cmd {
	return func() tea.Msg {
		ev, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return cloudInitImportStreamMsg{done: true}
			}
			return cloudInitImportStreamMsg{err: err, done: true}
		}
		switch e := ev.GetEvent().(type) {
		case *anvilv1.CloudInitImportRepoProgress_Status:
			return cloudInitImportStreamMsg{stream: stream, status: e.Status}
		case *anvilv1.CloudInitImportRepoProgress_Error:
			return cloudInitImportStreamMsg{err: fmt.Errorf("%s", e.Error), done: true}
		case *anvilv1.CloudInitImportRepoProgress_Imported:
			return cloudInitImportStreamMsg{stream: stream, result: e.Imported}
		default:
			return cloudInitImportStreamMsg{stream: stream}
		}
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
		return actionDoneMsg{screen: screenMirrors, verb: "added", err: err}
	}
}

func setMirrorEnabled(c *client.Client, name string, enabled bool) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Mirror.SetEnabled(context.Background(), &anvilv1.MirrorSetEnabledRequest{Name: name, Enabled: enabled})
		return actionDoneMsg{screen: screenMirrors, verb: "updated", err: err}
	}
}

func removeMirror(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Mirror.Remove(context.Background(), &anvilv1.MirrorRemoveRequest{Name: name})
		return actionDoneMsg{screen: screenMirrors, verb: "removed", err: err}
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
		return actionDoneMsg{screen: screenMigration, verb: "added", err: err}
	}
}

func removeHost(c *client.Client, alias string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Host.Remove(context.Background(), &anvilv1.HostRemoveRequest{Alias: alias})
		return actionDoneMsg{screen: screenMigration, verb: "removed", err: err}
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
