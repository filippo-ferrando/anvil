package commands

import (
	"context"
	"io"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestPrintPipeline(t *testing.T) {
	run := func(state string) *anvilv1.PipelineEvent {
		return &anvilv1.PipelineEvent{Event: &anvilv1.PipelineEvent_Run{Run: &anvilv1.PipelineRun{Id: "01R", State: state,
			Jobs: []*anvilv1.PipelineJob{{Name: "build", Distro: "u"}, {Name: "test", Distro: "u"}}}}}
	}
	events := []*anvilv1.PipelineEvent{
		run("running"),
		{Event: &anvilv1.PipelineEvent_Progress{Progress: &anvilv1.PipelineProgress{Job: "build", Distro: "u", Message: "restoring"}}},
		{Event: &anvilv1.PipelineEvent_Output{Output: &anvilv1.PipelineOutput{Job: "build", Distro: "u", Data: []byte("ok\n")}}},
		{Event: &anvilv1.PipelineEvent_Job{Job: &anvilv1.PipelineJob{Name: "build", Distro: "u", State: "failed", ExitCode: 2}}},
		{Event: &anvilv1.PipelineEvent_Job{Job: &anvilv1.PipelineJob{Name: "test", Distro: "u", State: "skipped", Error: "a job it needs failed"}}},
		{Event: &anvilv1.PipelineEvent_Progress{Progress: &anvilv1.PipelineProgress{Message: "applying"}}},
		run("failed"),
	}
	i := 0
	got, err := printPipeline(context.Background(), func() (*anvilv1.PipelineEvent, error) {
		if i == len(events) {
			return nil, io.EOF
		}
		i++
		return events[i-1], nil
	})
	if err != nil || got.GetState() != "failed" {
		t.Errorf("printPipeline = %+v, %v", got, err)
	}
	if err := finishPipeline(context.Background(), nil, got, ""); err == nil {
		t.Error("a failed run exited 0")
	}
	if err := finishPipeline(context.Background(), nil, &anvilv1.PipelineRun{State: "succeeded"}, ""); err != nil {
		t.Errorf("a passed run = %v", err)
	}
	if jobCounts(&anvilv1.PipelineRun{Jobs: []*anvilv1.PipelineJob{{State: "succeeded"}, {State: "failed"}, {State: "skipped"}}}) != "1/3 (1 failed)" {
		t.Error("jobCounts")
	}
	if shortCommit("0123456789abcdef") != "0123456789" || jobKey("a", "b") != "a@b" {
		t.Error("shortCommit or jobKey")
	}
}
