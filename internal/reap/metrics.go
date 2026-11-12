package reap

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
)

// Metrics holds reap counters and serves them in the Prometheus text
// exposition format. It is written by hand instead of pulling in
// prometheus/client_golang: four series do not justify the dependency.
//
// A nil *Metrics is valid and records nothing.
type Metrics struct {
	mu          sync.Mutex
	runs        uint64
	runErrors   uint64
	deleteErrs  uint64
	reapedByNS  map[string]uint64
	candidates  int
	lastSuccess float64 // unix seconds of the last run without errors
}

// NewMetrics returns an empty metrics set.
func NewMetrics() *Metrics {
	return &Metrics{reapedByNS: map[string]uint64{}}
}

// AddReaped counts one deleted Job in namespace ns.
func (m *Metrics) AddReaped(ns string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.reapedByNS[ns]++
	m.mu.Unlock()
}

// AddError counts one failed Job deletion.
func (m *Metrics) AddError() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.deleteErrs++
	m.mu.Unlock()
}

// ObserveRun records one finished sweep: how many candidates it planned,
// whether it failed, and when it ended (unix seconds).
func (m *Metrics) ObserveRun(candidates int, failed bool, endUnix float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs++
	m.candidates = candidates
	if failed {
		m.runErrors++
	} else {
		m.lastSuccess = endUnix
	}
}

// WriteTo writes all series in the Prometheus text format.
func (m *Metrics) WriteTo(w io.Writer) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var n int64
	p := func(format string, args ...any) {
		k, _ := fmt.Fprintf(w, format, args...)
		n += int64(k)
	}
	p("# HELP k8s_job_ttl_audit_reap_runs_total Reap sweeps run.\n")
	p("# TYPE k8s_job_ttl_audit_reap_runs_total counter\n")
	p("k8s_job_ttl_audit_reap_runs_total %d\n", m.runs)
	p("# HELP k8s_job_ttl_audit_reap_run_errors_total Reap sweeps that failed to list or had delete errors.\n")
	p("# TYPE k8s_job_ttl_audit_reap_run_errors_total counter\n")
	p("k8s_job_ttl_audit_reap_run_errors_total %d\n", m.runErrors)
	p("# HELP k8s_job_ttl_audit_reaped_jobs_total Jobs deleted by the reaper.\n")
	p("# TYPE k8s_job_ttl_audit_reaped_jobs_total counter\n")
	nss := make([]string, 0, len(m.reapedByNS))
	for ns := range m.reapedByNS {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	for _, ns := range nss {
		p("k8s_job_ttl_audit_reaped_jobs_total{namespace=%q} %d\n", ns, m.reapedByNS[ns])
	}
	p("# HELP k8s_job_ttl_audit_reap_delete_errors_total Job deletions that failed.\n")
	p("# TYPE k8s_job_ttl_audit_reap_delete_errors_total counter\n")
	p("k8s_job_ttl_audit_reap_delete_errors_total %d\n", m.deleteErrs)
	p("# HELP k8s_job_ttl_audit_reap_candidates Jobs eligible for reaping in the last sweep.\n")
	p("# TYPE k8s_job_ttl_audit_reap_candidates gauge\n")
	p("k8s_job_ttl_audit_reap_candidates %d\n", m.candidates)
	p("# HELP k8s_job_ttl_audit_reap_last_success_timestamp_seconds Unix time of the last sweep without errors.\n")
	p("# TYPE k8s_job_ttl_audit_reap_last_success_timestamp_seconds gauge\n")
	p("k8s_job_ttl_audit_reap_last_success_timestamp_seconds %g\n", m.lastSuccess)
	return n, nil
}

// ServeHTTP serves the metrics page.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = m.WriteTo(w)
}
