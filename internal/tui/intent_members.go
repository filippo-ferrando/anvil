package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// Labels of the n (create) and a (add members) forms on the Intents screen.
const (
	intentNameField       = "Name"
	vmMembersField        = "VM members"
	containerMembersField = "Container members"
)

// newIntentMembersForm is the form behind `anvil intent create` (create) or `anvil intent add`.
func newIntentMembersForm(create bool, intentName string) simpleForm {
	hypervisor := textField(hypervisorField, "qemu or firecracker, for the VM members", "qemu")
	hypervisor.Suggestions = []string{"qemu", "firecracker"}
	fields := []formField{
		textField(vmMembersField, "role:image, comma-separated, e.g. db:debian-13", ""),
		textField(containerMembersField, "role:image, comma-separated, e.g. cache:redis:7", ""),
		hypervisor,
	}
	title := "Add members to " + intentName
	if create {
		fields = append([]formField{textField(intentNameField, "name of the new intent", "")}, fields...)
		title = "Create intent"
	}
	return newSimpleForm(title, fields)
}

// newIntentMembersRequest reads the members form, VMs before containers like the CLI.
func newIntentMembersRequest(form simpleForm, create bool, intentName string) (*anvilv1.IntentAddMembersRequest, error) {
	if create {
		intentName = strings.TrimSpace(form.Value(intentNameField))
		if intentName == "" {
			return nil, errors.New("intent name is required")
		}
	}
	var members []*anvilv1.IntentMemberSpec
	for _, f := range []struct {
		label string
		kind  anvilv1.Kind
	}{{vmMembersField, anvilv1.Kind_KIND_VM}, {containerMembersField, anvilv1.Kind_KIND_CONTAINER}} {
		for _, spec := range splitCommaList(form.Value(f.label)) {
			m, err := client.ParseMemberSpec(f.kind, spec)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.label, err)
			}
			members = append(members, m)
		}
	}
	if len(members) == 0 {
		return nil, errors.New("at least one VM or container member is required")
	}
	hv, err := parseHypervisorField(form.Value(hypervisorField))
	if err != nil {
		return nil, err
	}
	return client.NewAddMembersRequest(intentName, create, members, hv)
}

// intentMembersTask launches req's members in the background. A failure still reloads
// the intents, since members launched before it are kept.
func (m *model) intentMembersTask(req *anvilv1.IntentAddMembersRequest) tea.Cmd {
	c, what := m.client, req.GetName()
	title, done := "adding members to "+what, "added members to "+what
	if req.GetCreate() {
		title, done = "creating intent "+what, "created intent "+what
	}
	_, cmd := m.streamTask(title, done, screenIntents, func(ctx context.Context, id int) tea.Cmd {
		wrap := lineTaskStep(id)
		return func() tea.Msg {
			stream, err := c.Intent.AddMembers(ctx, req)
			if err != nil {
				return wrap(failed[struct{}](err))
			}
			return recvStream(stream.Recv, parseIntentMembersProgress, wrap)()
		}
	})
	return cmd
}

// parseIntentMembersProgress turns one AddMembers event into a line, prefixed with "[role]"
// so each step keeps its own line.
func parseIntentMembersProgress(ev *anvilv1.IntentAddMembersProgress) lineStep {
	switch e := ev.GetEvent().(type) {
	case *anvilv1.IntentAddMembersProgress_Member:
		role := e.Member.GetRole()
		switch l := e.Member.GetLaunch().GetEvent().(type) {
		case *anvilv1.LaunchProgress_Status:
			return lineStep{status: "[" + role + "] " + l.Status}
		case *anvilv1.LaunchProgress_Instance:
			line := "[" + role + "] launched " + l.Instance.GetName()
			if ips := l.Instance.GetGuest().GetIpAddresses(); len(ips) > 0 {
				line += " (" + strings.Join(ips, ", ") + ")"
			}
			return lineStep{status: styleGood.Render(line)}
		}
	case *anvilv1.IntentAddMembersProgress_Error:
		return failed[struct{}](errors.New(e.Error))
	case *anvilv1.IntentAddMembersProgress_Intent:
		return lineStep{status: styleGood.Render(fmt.Sprintf("intent %s: %d member(s)", e.Intent.GetName(), len(e.Intent.GetMembers())))}
	}
	return lineStep{}
}
