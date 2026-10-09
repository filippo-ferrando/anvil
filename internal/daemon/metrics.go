package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/anvil-project/anvil/internal/instance"
)

// metric is one Prometheus gauge family and how to read it from a stats snapshot.
// ok reports whether the value is known for that instance.
type metric struct {
	name, help string
	value      func(instance.Stats) (v float64, ok bool)
}

func always(f func(instance.Stats) float64) func(instance.Stats) (float64, bool) {
	return func(s instance.Stats) (float64, bool) { return f(s), true }
}

var instanceMetrics = []metric{
	{"anvil_instance_cpu_percent", "CPU usage, relative to the instance's own CPU allocation.",
		always(func(s instance.Stats) float64 { return s.CPUPercent })},
	{"anvil_instance_memory_used_bytes", "Memory in use.",
		always(func(s instance.Stats) float64 { return float64(s.MemUsedBytes) })},
	{"anvil_instance_memory_limit_bytes", "Memory limit.",
		always(func(s instance.Stats) float64 { return float64(s.MemLimitBytes) })},
	{"anvil_instance_disk_used_bytes", "Disk space in use (VM only).",
		func(s instance.Stats) (float64, bool) { return float64(s.DiskUsedBytes), s.DiskTotalBytes > 0 }},
	{"anvil_instance_disk_total_bytes", "Disk size (VM only).",
		func(s instance.Stats) (float64, bool) { return float64(s.DiskTotalBytes), s.DiskTotalBytes > 0 }},
	{"anvil_instance_disk_read_bytes_per_second", "Disk read rate (container only).",
		always(func(s instance.Stats) float64 { return s.DiskReadBytesPerSec })},
	{"anvil_instance_disk_write_bytes_per_second", "Disk write rate (container only).",
		always(func(s instance.Stats) float64 { return s.DiskWriteBytesPerSec })},
	{"anvil_instance_network_receive_bytes_per_second", "Network receive rate.",
		func(s instance.Stats) (float64, bool) { return s.NetRxBytesPerSec, s.NetAvailable }},
	{"anvil_instance_network_transmit_bytes_per_second", "Network transmit rate.",
		func(s instance.Stats) (float64, bool) { return s.NetTxBytesPerSec, s.NetAvailable }},
	{"anvil_instance_uptime_seconds", "Time since the instance started.",
		always(func(s instance.Stats) float64 { return float64(s.UptimeSeconds) })},
}

// StatsSource is what the metrics endpoint needs from the instance manager.
type StatsSource interface {
	List(kind instance.Kind) ([]*instance.Spec, error)
	Stats(ctx context.Context, name string) (instance.Stats, error)
}

// MetricsHandler serves every instance's state and live stats in the Prometheus text format.
func MetricsHandler(src StatsSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		specs, err := src.List("")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Each Stats call samples for a short window, so running instances are read in parallel.
		stats := make([]*instance.Stats, len(specs))
		var wg sync.WaitGroup
		for i, spec := range specs {
			if spec.State != instance.StateRunning {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if s, err := src.Stats(r.Context(), spec.Name); err == nil {
					stats[i] = &s
				}
			}()
		}
		wg.Wait()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		writeMetrics(w, specs, stats)
	})
}

func writeMetrics(w io.Writer, specs []*instance.Spec, stats []*instance.Stats) {
	fmt.Fprintln(w, "# HELP anvil_instance_running Whether the instance is running (1) or not (0).")
	fmt.Fprintln(w, "# TYPE anvil_instance_running gauge")
	for _, spec := range specs {
		up := 0
		if spec.State == instance.StateRunning {
			up = 1
		}
		fmt.Fprintf(w, "anvil_instance_running%s %d\n", labels(spec), up)
	}
	for _, m := range instanceMetrics {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", m.name, m.help, m.name)
		for i, spec := range specs {
			if stats[i] == nil {
				continue
			}
			if v, ok := m.value(*stats[i]); ok {
				fmt.Fprintf(w, "%s%s %g\n", m.name, labels(spec), v)
			}
		}
	}
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func labels(spec *instance.Spec) string {
	return fmt.Sprintf(`{name="%s",kind="%s"}`, labelEscaper.Replace(spec.Name), spec.Kind)
}

// ServeMetrics serves MetricsHandler at /metrics on addr ("host:port", or "unix:/path"
// for a socket) until ctx ends.
func ServeMetrics(ctx context.Context, addr string, src StatsSource) error {
	network := "tcp"
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		network, addr = "unix", path
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	lis, err := net.Listen(network, addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", MetricsHandler(src))
	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	log.Printf("anvild: metrics on %s://%s/metrics", network, addr)
	if err := srv.Serve(lis); err != http.ErrServerClosed {
		return err
	}
	return nil
}
