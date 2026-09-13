// Package tui implements `anvil tui` (M8): a tview-based terminal UI,
// built on the exact same pkg/client wrapper the CLI uses — no business
// logic here either, per the plan's thin-client mandate, just a
// different way to drive the same RPCs `anvil launch`/`list`/`migrate`/
// etc. already do.
//
// One important, honest caveat: this was written with no way to actually
// run a terminal UI in this sandbox (no real TTY, and no network access
// to fetch the `github.com/rivo/tview`/`github.com/gdamore/tcell/v2`
// dependencies it needs — see the Makefile's own `tui-deps` target).
// It's built against tview's long-stable core widget API
// (Application/Pages/List/Table/Form/Flex/TextView/TextArea), reviewed
// carefully, but genuinely unexercised — a real `anvil tui` walkthrough
// on your own machine is what actually confirms this, same as the plan's
// own verification note for this milestone always said.
package tui

import (
	"context"
	"fmt"
	"os/user"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/pkg/client"
)

// App holds the tview application and the one daemon connection every
// view shares — same *client.Client the CLI dials, no separate
// connection-per-view.
type App struct {
	tapp   *tview.Application
	pages  *tview.Pages
	nav    *tview.List
	status *tview.TextView
	client *client.Client
	socket string

	instances *instancesView
	cloudInit *cloudInitView
	mirrors   *mirrorsView
	migration *migrationView
}

// Page names, referenced by both AddPage/SwitchToPage calls below and
// pageOrder, which lists them in nav display order — Overview is folded
// into the status line (local OS user + daemon connection health)
// rather than its own page, since that's all it would show.
const (
	pageInstances = "Instances"
	pageCloudInit = "Cloud Init"
	pageMirrors   = "Mirrors"
	pageMigration = "Migration"
)

var pageOrder = []string{pageInstances, pageCloudInit, pageMirrors, pageMigration}

// Run dials socketPath and blocks running the TUI until the user quits
// (q or Ctrl+C) or an unrecoverable error occurs.
func Run(socketPath string) error {
	c, err := client.Dial(socketPath)
	if err != nil {
		return fmt.Errorf("tui: dialing anvild at %s: %w", socketPath, err)
	}
	defer c.Close()

	a := &App{
		tapp:   tview.NewApplication(),
		pages:  tview.NewPages(),
		nav:    tview.NewList().ShowSecondaryText(false),
		status: tview.NewTextView().SetDynamicColors(true),
		client: c,
		socket: socketPath,
	}
	return a.run()
}

func (a *App) run() error {
	a.instances = newInstancesView(a)
	a.cloudInit = newCloudInitView(a)
	a.mirrors = newMirrorsView(a)
	a.migration = newMigrationView(a)

	a.pages.AddPage(pageInstances, a.instances.root, true, true)
	a.pages.AddPage(pageCloudInit, a.cloudInit.root, true, false)
	a.pages.AddPage(pageMirrors, a.mirrors.root, true, false)
	a.pages.AddPage(pageMigration, a.migration.root, true, false)

	a.nav.SetBorder(true).SetTitle(" anvil ")
	for _, name := range pageOrder {
		n := name // capture for the closure
		a.nav.AddItem(n, "", 0, func() {
			a.pages.SwitchToPage(n)
			a.tapp.SetFocus(a.currentView())
		})
	}
	a.nav.AddItem("Quit", "", 'q', func() { a.tapp.Stop() })

	a.updateStatus("connecting…")
	a.refreshStatus()

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(
			tview.NewFlex().SetDirection(tview.FlexColumn).
				AddItem(a.nav, 22, 0, true).
				AddItem(a.pages, 0, 1, false),
			0, 1, true,
		).
		AddItem(a.status, 1, 0, false)

	a.tapp.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlC {
			a.tapp.Stop()
			return nil
		}
		return event
	})

	return a.tapp.SetRoot(root, true).SetFocus(a.nav).Run()
}

// currentView returns whichever view's root primitive the nav just
// switched to, so focus moves there instead of staying on the nav list —
// same shape every view.root already has (each is a *tview.Flex or
// similar Primitive).
func (a *App) currentView() tview.Primitive {
	_, prim := a.pages.GetFrontPage()
	return prim
}

// updateStatus sets the status line's right-hand detail text, keeping
// the left-hand "user@host" identity part intact.
func (a *App) updateStatus(detail string) {
	who := "unknown"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	a.status.SetText(fmt.Sprintf("[grey]%s  socket=%s  %s[-]", who, a.socket, detail))
}

// refreshStatus does a cheap round-trip (List with a filter that matches
// nothing meaningful, just to confirm the socket answers) to show
// "connected" vs "unreachable" — cheap, non-blocking on the UI thread.
func (a *App) refreshStatus() {
	go func() {
		_, err := a.client.List(context.Background(), &anvilv1.ListRequest{})
		a.tapp.QueueUpdateDraw(func() {
			if err != nil {
				a.updateStatus("[red]daemon unreachable[-]")
				return
			}
			a.updateStatus("[green]connected[-]")
		})
	}()
}

// showError pushes a modal with msg, dismissed by any key — used by every
// view for a failed RPC instead of silently swallowing it.
func (a *App) showError(title, msg string) {
	modal := tview.NewModal().
		SetText(fmt.Sprintf("%s\n\n%s", title, msg)).
		AddButtons([]string{"OK"}).
		SetDoneFunc(func(int, string) { a.pages.RemovePage("error") })
	a.pages.AddPage("error", modal, true, true)
}
