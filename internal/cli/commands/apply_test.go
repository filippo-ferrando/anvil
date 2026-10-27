package commands

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestShouldConfirmApply(t *testing.T) {
	tty := true
	orig := applyInteractive
	applyInteractive = func(io.Writer) bool { return tty }
	t.Cleanup(func() { applyInteractive = orig })

	tests := []struct {
		name   string
		dryRun bool
		yes    bool
		file   string
		tty    bool
		want   bool
	}{
		{"terminal asks", false, false, "anvil.yaml", true, true},
		{"not a terminal applies as before", false, false, "anvil.yaml", false, false},
		{"--yes skips", false, true, "anvil.yaml", true, false},
		{"--dry-run never asks", true, false, "anvil.yaml", true, false},
		{"manifest on stdin never asks", false, false, "-", true, false},
	}
	for _, tt := range tests {
		tty = tt.tty
		if got := shouldConfirmApply(tt.dryRun, tt.yes, tt.file, io.Discard); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestConfirm(t *testing.T) {
	for answer, want := range map[string]bool{"y\n": true, "YES\n": true, " y \n": true, "n\n": false, "\n": false, "": false, "maybe\n": false} {
		var out bytes.Buffer
		if got := confirm(strings.NewReader(answer), &out, "Go?"); got != want {
			t.Errorf("answer %q: got %v, want %v", answer, got, want)
		}
		if out.String() != "Go? [y/N] " {
			t.Errorf("prompt = %q", out.String())
		}
	}
}

func planEvent(actions ...string) *anvilv1.IntentApplyProgress {
	plan := &anvilv1.IntentApplyPlan{IntentName: "web"}
	for _, a := range actions {
		plan.Steps = append(plan.Steps, &anvilv1.IntentApplyStep{Role: "db", InstanceName: "web-db", Action: a})
	}
	return &anvilv1.IntentApplyProgress{Event: &anvilv1.IntentApplyProgress_Plan{Plan: plan}}
}

var appliedEvent = &anvilv1.IntentApplyProgress{Event: &anvilv1.IntentApplyProgress_Intent{Intent: &anvilv1.Intent{Name: "web"}}}

// fakeApplyStarter records which kind of apply each call started.
func fakeApplyStarter(calls *[]bool, plan *anvilv1.IntentApplyProgress) func(bool) (applyStream, error) {
	return func(dry bool) (applyStream, error) {
		*calls = append(*calls, dry)
		events := []*anvilv1.IntentApplyProgress{plan}
		if !dry {
			events = append(events, appliedEvent)
		}
		return &fakeApplyStream{events: events}, nil
	}
}

func TestRunApply(t *testing.T) {
	yes := func() bool { return true }
	no := func() bool { return false }

	tests := []struct {
		name      string
		plan      *anvilv1.IntentApplyProgress
		ask       func() bool
		wantCalls []bool
		wantPlans int
		wantApply bool
	}{
		{"no prompt applies directly", planEvent("create"), nil, []bool{false}, 1, true},
		{"confirmed applies after a dry run", planEvent("create", "keep"), yes, []bool{true, false}, 1, true},
		{"declined stops after the dry run", planEvent("update"), no, []bool{true}, 1, false},
		{"no changes applies without asking", planEvent("keep"), func() bool { t.Error("asked for a no-op plan"); return false }, []bool{true, false}, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []bool
			var out bytes.Buffer
			if err := runApply(&out, fakeApplyStarter(&calls, tt.plan), tt.ask); err != nil {
				t.Fatal(err)
			}
			if len(calls) != len(tt.wantCalls) {
				t.Fatalf("calls (dry run per call) = %v, want %v", calls, tt.wantCalls)
			}
			for i := range calls {
				if calls[i] != tt.wantCalls[i] {
					t.Fatalf("calls (dry run per call) = %v, want %v", calls, tt.wantCalls)
				}
			}
			if n := strings.Count(out.String(), "Plan for intent"); n != tt.wantPlans {
				t.Errorf("plan printed %d times, want %d:\n%s", n, tt.wantPlans, out.String())
			}
			if got := strings.Contains(out.String(), "Applied intent"); got != tt.wantApply {
				t.Errorf("applied = %v, want %v:\n%s", got, tt.wantApply, out.String())
			}
		})
	}

	t.Run("a failed dry run is returned", func(t *testing.T) {
		start := func(bool) (applyStream, error) { return nil, errors.New("boom") }
		if err := runApply(io.Discard, start, yes); err == nil || err.Error() != "boom" {
			t.Fatalf("err = %v", err)
		}
	})
}
