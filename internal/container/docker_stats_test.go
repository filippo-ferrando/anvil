package container

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/container/docker"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// startStatsDocker serves growing stats counters for every container; reset starts them over.
func startStatsDocker(t *testing.T) (*docker.Client, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	dir, err := os.MkdirTemp("", "anvil-docker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var reset atomic.Bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/stats"):
			n := uint64(calls.Add(1))
			if reset.Load() {
				n = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": n * 1e8}, "system_cpu_usage": n * 1e9, "online_cpus": 2},
				"networks":  map[string]any{"eth0": map[string]any{"rx_bytes": n * 1000, "tx_bytes": n * 10}},
			})
		case strings.HasSuffix(r.URL.Path, "/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"State": map[string]any{"Running": true, "StartedAt": time.Now().Add(-time.Minute).Format(time.RFC3339Nano)}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return docker.NewClient(sock), &calls, &reset
}

func TestDockerSampleStats(t *testing.T) {
	client, calls, reset := startStatsDocker(t)
	b := &DockerBackend{Client: client}
	spec := &instance.Spec{Name: "web", Container: &instance.ContainerSpec{ContainerID: "c1"}}
	ctx := context.Background()

	start := time.Now()
	st, prev, err := b.SampleStats(ctx, spec, nil)
	if err != nil || time.Since(start) < statsSampleWindow || calls.Load() != 2 {
		t.Fatalf("first sample = %+v, %v after %s with %d stats calls", st, err, time.Since(start), calls.Load())
	}
	if st.CPUPercent != 20 || st.NetRxBytesPerSec <= 0 || st.UptimeSeconds < 59 {
		t.Errorf("first sample = %+v", st)
	}
	start = time.Now()
	st, prev, err = b.SampleStats(ctx, spec, prev)
	if err != nil || time.Since(start) > statsSampleWindow/2 || calls.Load() != 3 || st.CPUPercent != 20 {
		t.Errorf("sample since prev = %+v, %v after %s with %d stats calls", st, err, time.Since(start), calls.Load())
	}
	// Counters that started over (a restarted container) give no rates, not huge ones.
	reset.Store(true)
	if st, _, err = b.SampleStats(ctx, spec, prev); err != nil || st.CPUPercent != 0 || st.NetRxBytesPerSec != 0 {
		t.Errorf("after a restart = %+v, %v", st, err)
	}
	// Another container's sample isn't used.
	other := prev.(containerSample)
	other.id = "c2"
	before := calls.Load()
	if _, _, err := b.SampleStats(ctx, spec, other); err != nil || calls.Load()-before != 2 {
		t.Errorf("a stale sample was used: %v, %d calls", err, calls.Load()-before)
	}
}
