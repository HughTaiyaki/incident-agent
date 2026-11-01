package httpapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/hughtaiyaki/incident-agent/internal/app/runstate"
)

// metrics renders the Prometheus text format by hand: a dozen store-derived values
// do not justify a client library. Values describe the deployment, so an API replica
// can serve them; a Redis failure is itself a reportable value, not an error.
func (a *api) metrics(w http.ResponseWriter, r *http.Request) {
	stats, err := a.deps.Stats(r.Context())
	var out strings.Builder
	write := func(name, kind, help string, lines ...string) {
		_, _ = fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
		for _, line := range lines {
			_, _ = fmt.Fprintf(&out, "%s%s\n", name, line)
		}
	}
	if err != nil {
		write("incident_redis_up", "gauge", "Whether the state store answered this scrape.", " 0")
	} else {
		write("incident_redis_up", "gauge", "Whether the state store answered this scrape.", " 1")
		writeStats(write, stats)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(out.String()))
}

func writeStats(write func(name, kind, help string, lines ...string), stats runstate.Stats) {
	alive := 0
	if stats.WorkerAlive {
		alive = 1
	}
	write("incident_worker_alive", "gauge", "Whether a worker heartbeat is current.", fmt.Sprintf(" %d", alive))
	write("incident_runs_active", "gauge", "Runs holding an admission slot.",
		fmt.Sprintf(`{state="ready"} %d`, stats.Ready), fmt.Sprintf(`{state="leased"} %d`, stats.Leased))
	write("incident_runs_expired_leases", "gauge", "Leased runs whose lease has expired without takeover.", fmt.Sprintf(" %d", stats.ExpiredLeases))
	write("incident_oldest_ready_age_seconds", "gauge", "Age of the oldest queued run.", fmt.Sprintf(" %g", float64(stats.OldestReadyMS)/1000))
	write("incident_redis_memory_used_bytes", "gauge", "Redis used_memory.", fmt.Sprintf(" %d", stats.UsedMemory))
	write("incident_redis_memory_max_bytes", "gauge", "Redis maxmemory.", fmt.Sprintf(" %d", stats.MaxMemory))
	finished := []string{}
	for key, value := range stats.Counters {
		if status, ok := strings.CutPrefix(key, "runs:"); ok {
			finished = append(finished, fmt.Sprintf(`{status=%q} %d`, status, value))
		}
	}
	slices.Sort(finished)
	write("incident_runs_finished_total", "counter", "Finished runs; best effort, may undercount after a worker crash.", finished...)
	write("incident_runs_blocked_total", "counter", "Times a worker reported a run it could not process.", fmt.Sprintf(" %d", stats.Counters["runs_blocked"]))
	write("incident_model_calls_total", "counter", "Model calls of finished runs.", fmt.Sprintf(" %d", stats.Counters["model_calls"]))
	write("incident_model_tokens_total", "counter", "Tokens charged to finished runs.", fmt.Sprintf(" %d", stats.Counters["charged_tokens"]))
	write("incident_backend_requests_total", "counter", "Telemetry backend requests of finished runs.", fmt.Sprintf(" %d", stats.Counters["backend_requests"]))
}
