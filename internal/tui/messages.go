package tui

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

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

type containerImagesLoadedMsg struct {
	images []*anvilv1.ContainerImage
	err    error
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

// snapshotsLoadedMsg carries one instance's snapshot list back from a
// SnapshotService.List call. instanceName lets the handler ignore a stale
// reply that arrives after the user has since selected a different instance.
type snapshotsLoadedMsg struct {
	instanceName string
	snapshots    []*anvilv1.SnapshotInfo
	err          error
}

// exportStreamMsg carries one event off an ExportService.Export stream —
// shared by the Instances and Intents screens, whichever started it.
type exportStreamMsg struct {
	stream anvilv1.ExportService_ExportClient
	line   string
	err    error
	done   bool
}

// importStreamMsg carries one event off an ExportService.Import stream.
type importStreamMsg struct {
	stream anvilv1.ExportService_ImportClient
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

func loadContainerImages(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Image.ListContainerImages(context.Background(), &anvilv1.ContainerImageListRequest{})
		if err != nil {
			return containerImagesLoadedMsg{err: err}
		}
		return containerImagesLoadedMsg{images: reply.GetImages()}
	}
}

func deleteContainerImage(c *client.Client, id string, engine anvilv1.ContainerEngine) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Image.DeleteContainerImage(context.Background(), &anvilv1.ContainerImageDeleteRequest{Id: id, Engine: engine})
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

// startExportStream begins `anvil export name -o outputPath`. isIntent
// skips instance resolution and looks name up as an intent only — pass
// true from the Intents page, which already knows name is an intent and
// shouldn't be shadowed by an unrelated instance of the same name.
func startExportStream(c *client.Client, name, outputPath string, isIntent bool) tea.Cmd {
	return func() tea.Msg {
		absOutput, err := filepath.Abs(outputPath)
		if err != nil {
			return exportStreamMsg{err: err, done: true}
		}
		stream, err := c.Export.Export(context.Background(), &anvilv1.ExportRequest{Name: name, OutputPath: absOutput, IsIntent: isIntent})
		if err != nil {
			return exportStreamMsg{err: err, done: true}
		}
		return receiveExportEvent(stream)()
	}
}

func receiveExportEvent(stream anvilv1.ExportService_ExportClient) tea.Cmd {
	return func() tea.Msg {
		ev, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return exportStreamMsg{done: true}
			}
			return exportStreamMsg{err: err, done: true}
		}
		switch e := ev.GetEvent().(type) {
		case *anvilv1.ExportProgress_Status:
			return exportStreamMsg{stream: stream, line: e.Status}
		case *anvilv1.ExportProgress_Error:
			return exportStreamMsg{err: fmt.Errorf("%s", e.Error), done: true}
		case *anvilv1.ExportProgress_Done:
			return exportStreamMsg{stream: stream, line: styleGood.Render("exported: " + e.Done)}
		default:
			return exportStreamMsg{stream: stream}
		}
	}
}

// startImportStream begins `anvil import bundlePath [--name renameTo]` and
// streams its progress back.
func startImportStream(c *client.Client, bundlePath, renameTo string) tea.Cmd {
	return func() tea.Msg {
		absBundle, err := filepath.Abs(bundlePath)
		if err != nil {
			return importStreamMsg{err: err, done: true}
		}
		stream, err := c.Export.Import(context.Background(), &anvilv1.ImportRequest{BundlePath: absBundle, Name: renameTo})
		if err != nil {
			return importStreamMsg{err: err, done: true}
		}
		return receiveImportEvent(stream)()
	}
}

func receiveImportEvent(stream anvilv1.ExportService_ImportClient) tea.Cmd {
	return func() tea.Msg {
		ev, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return importStreamMsg{done: true}
			}
			return importStreamMsg{err: err, done: true}
		}
		switch e := ev.GetEvent().(type) {
		case *anvilv1.ImportProgress_Status:
			return importStreamMsg{stream: stream, line: e.Status}
		case *anvilv1.ImportProgress_Error:
			return importStreamMsg{err: fmt.Errorf("%s", e.Error), done: true}
		case *anvilv1.ImportProgress_Done:
			done := e.Done
			var b strings.Builder
			if done.GetIntentName() != "" {
				b.WriteString(styleGood.Render("imported intent " + done.GetIntentName()))
				for _, mr := range done.GetMembers() {
					b.WriteString("\n  " + mr.GetRole() + ": " + mr.GetNewId())
				}
			} else if len(done.GetMembers()) > 0 {
				b.WriteString(styleGood.Render("imported: " + done.GetMembers()[0].GetNewId()))
			}
			return importStreamMsg{stream: stream, line: b.String()}
		default:
			return importStreamMsg{stream: stream}
		}
	}
}

func addPort(c *client.Client, name string, hostPort, guestPort int, protocol string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.AddPort(context.Background(), &anvilv1.AddPortRequest{
			Name: name,
			Port: &anvilv1.PortMapping{HostPort: int32(hostPort), GuestPort: int32(guestPort), Protocol: protocol},
		})
		return actionDoneMsg{screen: screenInstances, verb: "port added", err: err}
	}
}

func removePort(c *client.Client, name string, hostPort int, protocol string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.RemovePort(context.Background(), &anvilv1.RemovePortRequest{
			Name: name, HostPort: int32(hostPort), Protocol: protocol,
		})
		return actionDoneMsg{screen: screenInstances, verb: "port removed", err: err}
	}
}

func loadSnapshots(c *client.Client, instanceName string) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Snapshot.List(context.Background(), &anvilv1.SnapshotListRequest{Name: instanceName})
		if err != nil {
			return snapshotsLoadedMsg{instanceName: instanceName, err: err}
		}
		return snapshotsLoadedMsg{instanceName: instanceName, snapshots: reply.GetSnapshots()}
	}
}

func createSnapshot(c *client.Client, instanceName, snapshotName string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Snapshot.Create(context.Background(), &anvilv1.SnapshotCreateRequest{Name: instanceName, SnapshotName: snapshotName})
		return actionDoneMsg{screen: screenSnapshots, verb: "snapshot created", err: err}
	}
}

func restoreSnapshot(c *client.Client, instanceName, snapshotName string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Snapshot.Restore(context.Background(), &anvilv1.SnapshotRestoreRequest{Name: instanceName, SnapshotName: snapshotName})
		return actionDoneMsg{screen: screenSnapshots, verb: "snapshot restored", err: err}
	}
}

func deleteSnapshot(c *client.Client, instanceName, snapshotName string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Snapshot.Delete(context.Background(), &anvilv1.SnapshotDeleteRequest{Name: instanceName, SnapshotName: snapshotName})
		return actionDoneMsg{screen: screenSnapshots, verb: "snapshot deleted", err: err}
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
