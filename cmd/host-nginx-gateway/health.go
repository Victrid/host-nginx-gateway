// Minimal healthz/metrics endpoint (DESIGN.md §1: "/healthz + /metrics
// 端点（reload 延迟/失败计数）"). Served on --healthz-addr by a plain
// net/http server; controller-runtime's own metrics/health servers are
// disabled so there is exactly one listener.
//
// Counters are deliberately hand-rolled (sync/atomic + Prometheus text
// exposition) instead of pulling the prometheus client into cmd/: the
// surface is three integers.
package main

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Metrics aggregates the counters exposed on /metrics. Safe for concurrent
// use. The provider calls Applier.Apply exactly once per full sync, so
// Reconciles doubles as the full-sync counter (DESIGN.md §7).
type Metrics struct {
	Reconciles      atomic.Int64
	ReloadSuccesses atomic.Int64
	ReloadFailures  atomic.Int64
}

// Handler returns the http.Handler serving /healthz and /metrics.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", m.handleHealthz)
	mux.HandleFunc("/metrics", m.handleMetrics)
	return mux
}

func (m *Metrics) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Liveness = the process is up and serving. Readiness refinement
	// (first successful sync) is future work; the kubelet liveness probe
	// and this endpoint watch the process.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

func (m *Metrics) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP hng_reconciles_total Total number of full syncs run by the controller.\n"+
		"# TYPE hng_reconciles_total counter\n"+
		"hng_reconciles_total %d\n", m.Reconciles.Load())
	_, _ = fmt.Fprintf(w, "# HELP hng_dataplane_applies_total Dataplane apply outcomes (render + validate + reload).\n"+
		"# TYPE hng_dataplane_applies_total counter\n"+
		"hng_dataplane_applies_total{result=\"success\"} %d\n"+
		"hng_dataplane_applies_total{result=\"failure\"} %d\n",
		m.ReloadSuccesses.Load(), m.ReloadFailures.Load())
}
