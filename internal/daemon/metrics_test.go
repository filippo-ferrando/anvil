package daemon

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anvil-project/anvil/internal/instance"
)

type fakeStats struct{}

func (fakeStats) List(instance.Kind) ([]*instance.Spec, error) {
	return []*instance.Spec{
		{Name: "web", Kind: instance.KindVM, State: instance.StateRunning},
		{Name: "db", Kind: instance.KindContainer, State: instance.StateStopped},
	}, nil
}

func (fakeStats) Stats(_ context.Context, name string) (instance.Stats, error) {
	return instance.Stats{CPUPercent: 12.5, MemUsedBytes: 1024, UptimeSeconds: 60}, nil
}

func TestMetricsHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	MetricsHandler(fakeStats{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	for _, want := range []string{
		`anvil_instance_running{name="web",kind="vm"} 1`,
		`anvil_instance_running{name="db",kind="container"} 0`,
		`anvil_instance_cpu_percent{name="web",kind="vm"} 12.5`,
		`anvil_instance_memory_used_bytes{name="web",kind="vm"} 1024`,
		"# TYPE anvil_instance_uptime_seconds gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A stopped instance has no live stats, and unknown values are left out.
	for _, bad := range []string{`cpu_percent{name="db"`, "anvil_instance_disk_total_bytes{", "network_receive_bytes_per_second{"} {
		if strings.Contains(out, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, out)
		}
	}
}
