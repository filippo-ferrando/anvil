package commands

import (
	"bytes"
	"encoding/json"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestPrintJSON(t *testing.T) {
	var buf bytes.Buffer
	err := printJSON(&buf, &anvilv1.ListReply{Instances: []*anvilv1.Instance{
		{Name: "web", State: anvilv1.State_STATE_RUNNING, Labels: map[string]string{"intent": "shop"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Instances []struct {
			Name   string            `json:"name"`
			State  string            `json:"state"`
			Labels map[string]string `json:"labels"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, buf.String())
	}
	if len(got.Instances) != 1 || got.Instances[0].Name != "web" || got.Instances[0].State != "STATE_RUNNING" || got.Instances[0].Labels["intent"] != "shop" {
		t.Fatalf("got %+v", got)
	}
}

func TestCheckOutput(t *testing.T) {
	for _, ok := range []string{"table", "json"} {
		if err := checkOutput(ok); err != nil {
			t.Errorf("checkOutput(%q): %v", ok, err)
		}
	}
	if err := checkOutput("yaml"); err == nil {
		t.Error(`checkOutput("yaml"): expected an error`)
	}
}
