package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/apply"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

type manifestItem struct{ info *anvilv1.ManifestInfo }

func (i manifestItem) FilterValue() string { return i.info.GetName() }
func (i manifestItem) Title() string       { return i.info.GetName() }
func (i manifestItem) Description() string {
	if i.info.GetIntent() == "" {
		return "no intent: field"
	}
	return "intent " + i.info.GetIntent()
}

type manifestPrompt int

const (
	manifestPromptNone manifestPrompt = iota
	manifestPromptNew
	manifestPromptImport
	manifestPromptExport
	manifestPromptRename
	manifestPromptDelete
	manifestPromptApply
	manifestPromptTeardown
)

// manifestsModel is the Manifests page: a saved anvil.yaml library with an editor, apply and teardown.
type manifestsModel struct {
	splitEditor
	intent   string // the loaded manifest's intent: field
	prompt   manifestPrompt
	promptFm simpleForm
	apply    applyRun
}

func newManifestsModel() manifestsModel {
	return manifestsModel{
		splitEditor: newSplitEditor("Manifests", "select or create a manifest to edit its anvil.yaml…"),
		apply:       applyRun{owner: screenManifests},
	}
}

type manifestListLoadedMsg struct {
	manifests []*anvilv1.ManifestInfo
	err       error
}

type manifestContentLoadedMsg struct {
	name, intent, content string
	err                   error
}

type manifestSavedMsg struct {
	name string
	err  error
}

func loadManifestList(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Manifest.List(context.Background(), &anvilv1.ManifestListRequest{})
		return manifestListLoadedMsg{manifests: reply.GetManifests(), err: err}
	}
}

func loadManifestContent(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Manifest.Get(context.Background(), &anvilv1.ManifestGetRequest{Name: name})
		return manifestContentLoadedMsg{name: name, intent: reply.GetInfo().GetIntent(), content: reply.GetContent(), err: err}
	}
}

func saveManifest(c *client.Client, name, content string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Manifest.Save(context.Background(), &anvilv1.ManifestSaveRequest{Name: name, Content: content})
		return manifestSavedMsg{name: name, err: err}
	}
}

func renameManifest(c *client.Client, oldName, newName string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Manifest.Rename(context.Background(), &anvilv1.ManifestRenameRequest{OldName: oldName, NewName: newName})
		return manifestSavedMsg{name: newName, err: err}
	}
}

func deleteManifest(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Manifest.Delete(context.Background(), &anvilv1.ManifestDeleteRequest{Name: name})
		return actionDoneMsg{screen: screenManifests, verb: "deleted", err: err}
	}
}

func (m model) updateManifests(msg tea.Msg) (tea.Model, tea.Cmd) {
	mf := &m.manifests
	switch msg := msg.(type) {
	case manifestListLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing manifests: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.manifests))
		for i, info := range msg.manifests {
			items[i] = manifestItem{info: info}
		}
		mf.list.SetItems(items)
		return m, nil

	case manifestContentLoadedMsg:
		if msg.err != nil {
			m.setStatus("loading "+msg.name+": "+msg.err.Error(), true)
			return m, nil
		}
		mf.current, mf.intent, mf.dirty = msg.name, msg.intent, false
		mf.editor.SetValue(msg.content)
		return m, nil

	case manifestSavedMsg:
		if msg.err != nil {
			m.setStatus("saving "+msg.name+": "+msg.err.Error(), true)
			return m, nil
		}
		m.setStatus("saved "+msg.name, false)
		// Reload so the intent: field shown and used by teardown matches what was saved.
		return m, tea.Batch(loadManifestList(m.client), loadManifestContent(m.client, msg.name))

	case actionDoneMsg:
		if msg.err != nil {
			m.setStatus("manifest "+msg.verb+": "+msg.err.Error(), true)
			return m, nil
		}
		mf.current, mf.intent = "", ""
		mf.editor.SetValue("")
		m.setStatus("manifest "+msg.verb, false)
		return m, loadManifestList(m.client)

	case applyStreamMsg:
		cmd, applied := mf.apply.handle(msg)
		if applied {
			m.setStatus("applied "+mf.current, false)
		}
		return m, cmd

	case tea.KeyMsg:
		return m.updateManifestsKey(msg)
	}
	return m, nil
}

func (m model) updateManifestsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	mf := &m.manifests

	if mf.apply.active() {
		return m, mf.apply.key(msg, m.client)
	}
	if mf.prompt != manifestPromptNone {
		return m.updateManifestsPrompt(msg)
	}

	if mf.editing {
		switch msg.String() {
		case "esc":
			mf.editing = false
			mf.editor.Blur()
			return m, nil
		case "ctrl+s":
			return m, saveManifest(m.client, mf.current, mf.editor.Value())
		}
		var cmd tea.Cmd
		mf.editor, cmd = mf.editor.Update(msg)
		mf.dirty = true
		return m, cmd
	}

	// Keys below act on the loaded manifest, which needs one to be loaded.
	needCurrent := func() bool {
		if mf.current == "" {
			m.setStatus("select a manifest first (enter)", true)
			return false
		}
		return true
	}
	switch msg.String() {
	case "esc", "q":
		m.sidebarFocused = true
		return m, nil
	case "n":
		mf.prompt = manifestPromptNew
		mf.promptFm = newSimpleForm("New manifest", []formField{textField("Name", "also used as the intent name", "")})
		return m, nil
	case "m":
		mf.prompt = manifestPromptImport
		mf.promptFm = newSimpleForm("Import manifest", []formField{
			pathField("Local file path", "e.g. ./anvil.yaml", ""),
			textField("Name", "defaults to the file name", ""),
		})
		return m, nil
	case "enter", "tab":
		if item, ok := mf.list.SelectedItem().(manifestItem); ok {
			return m, loadManifestContent(m.client, item.info.GetName())
		}
		return m, nil
	case "e":
		if needCurrent() {
			mf.editing = true
			return m, mf.editor.Focus()
		}
	case "E":
		if needCurrent() {
			mf.prompt = manifestPromptExport
			mf.promptFm = newSimpleForm("Export "+mf.current, []formField{pathField("Output path", "", mf.current+".yaml")})
		}
	case "r":
		if needCurrent() {
			mf.prompt = manifestPromptRename
			mf.promptFm = newSimpleForm("Rename "+mf.current, []formField{textField("New name", "", "")})
		}
	case "d":
		if needCurrent() {
			mf.prompt = manifestPromptDelete
		}
	case "a":
		if !needCurrent() {
			return m, nil
		}
		if mf.dirty {
			m.setStatus("unsaved changes: save with ctrl+s before applying", true)
			return m, nil
		}
		mf.prompt = manifestPromptApply
		mf.promptFm = newSimpleForm("Apply "+mf.current, []formField{
			textField("Vars (optional)", "KEY=VALUE,KEY2=VALUE2, over the shell env, .env and vars:", ""),
			toggleField("Prune", "delete members no longer in the manifest", false),
			toggleField("Recreate VMs", "allow replacing a VM, its disk is lost", false),
		})
	case "x":
		if !needCurrent() {
			return m, nil
		}
		if mf.intent == "" {
			m.setStatus(mf.current+" has no intent: field", true)
			return m, nil
		}
		mf.prompt = manifestPromptTeardown
	default:
		var cmd tea.Cmd
		mf.list, cmd = mf.list.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) updateManifestsPrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	mf := &m.manifests

	switch mf.prompt {
	case manifestPromptDelete:
		mf.prompt = manifestPromptNone
		if k := msg.String(); k == "y" || k == "enter" {
			return m, deleteManifest(m.client, mf.current)
		}
		return m, nil
	case manifestPromptTeardown:
		mf.prompt = manifestPromptNone
		switch msg.String() {
		case "y", "enter":
			return m, deleteIntent(m.client, mf.intent, true)
		case "u":
			return m, deleteIntent(m.client, mf.intent, false)
		}
		return m, nil
	}

	var submitted, cancelled bool
	mf.promptFm, submitted, cancelled = mf.promptFm.update(msg)
	if cancelled {
		mf.prompt = manifestPromptNone
	}
	if !submitted {
		return m, nil
	}
	prompt := mf.prompt
	mf.prompt = manifestPromptNone

	switch prompt {
	case manifestPromptNew:
		if name := strings.TrimSpace(mf.promptFm.Value("Name")); name != "" {
			return m, saveManifest(m.client, name, apply.Starter(name))
		}

	case manifestPromptImport:
		path, name := mf.promptFm.Value("Local file path"), strings.TrimSpace(mf.promptFm.Value("Name"))
		if path == "" {
			return m, nil
		}
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			m.setStatus("reading "+path+": "+err.Error(), true)
			return m, nil
		}
		return m, saveManifest(m.client, name, string(data))

	case manifestPromptExport:
		path := mf.promptFm.Value("Output path")
		if path == "" {
			return m, nil
		}
		if err := os.WriteFile(path, []byte(mf.editor.Value()), 0o644); err != nil {
			m.setStatus("writing "+path+": "+err.Error(), true)
		} else {
			m.setStatus("exported "+mf.current+" to "+path, false)
		}

	case manifestPromptRename:
		if newName := strings.TrimSpace(mf.promptFm.Value("New name")); newName != "" {
			return m, renameManifest(m.client, mf.current, newName)
		}

	case manifestPromptApply:
		req, err := client.NewApplyRequest("", []byte(mf.editor.Value()), client.ApplyOptions{
			Prune:    mf.promptFm.Bool("Prune"),
			Recreate: mf.promptFm.Bool("Recreate VMs"),
			Vars:     splitCommaList(mf.promptFm.Value("Vars (optional)")),
		})
		if err != nil {
			m.setStatus(err.Error(), true)
			return m, nil
		}
		return m, mf.apply.start(m.client, req)
	}
	return m, nil
}

func (m manifestsModel) View() string {
	if m.apply.active() {
		return m.apply.view()
	}
	switch m.prompt {
	case manifestPromptNone:
	case manifestPromptDelete:
		return styleWarn.Render("Delete saved manifest "+m.current+"? A running intent is left alone.") + "\n\n" +
			helpBar("y", "confirm", "any other key", "cancel")
	case manifestPromptTeardown:
		return styleWarn.Render("Tear down intent "+m.intent+"?") + "\n\n" +
			helpBar("y", "delete the intent and its members", "u", "ungroup only, keep members", "any other key", "cancel")
	default:
		return m.promptFm.View()
	}
	help := helpBar("n", "new", "m", "import", "E", "export", "e", "edit", "ctrl+s", "save", "a", "apply",
		"x", "tear down", "r", "rename", "d", "delete", "esc", "back")
	return m.panels() + "\n" + help
}
