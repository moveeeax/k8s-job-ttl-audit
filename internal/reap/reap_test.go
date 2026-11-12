package reap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/moveeeax/k8s-job-ttl-audit/internal/audit"
)

var refNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func finishedAgo(name, ns string, d time.Duration) audit.Job {
	t := refNow.Add(-d)
	return audit.Job{Name: name, Namespace: ns, UID: "uid-" + ns + "-" + name,
		Finished: true, FinishedReason: "Complete", CompletionTime: &t}
}

func cronJobAgo(name, owner string, d time.Duration) audit.Job {
	j := finishedAgo(name, "batch", d)
	j.OwnerKind, j.OwnerName = "CronJob", owner
	return j
}

func names(p Plan) string {
	var out []string
	for _, c := range p.Candidates {
		out = append(out, c.Namespace+"/"+c.Name)
	}
	return strings.Join(out, ",")
}

func TestPlanRetentionWindow(t *testing.T) {
	jobs := []audit.Job{
		finishedAgo("old", "app", 200*time.Hour),
		finishedAgo("edge", "app", 168*time.Hour), // exactly at window: kept
		finishedAgo("young", "app", time.Hour),
		{Name: "running", Namespace: "app", UID: "r"},
	}
	p := PlanReap(jobs, Options{OlderThan: 168 * time.Hour, Now: refNow})
	if got := names(p); got != "app/old" {
		t.Fatalf("candidates = %s, want app/old", got)
	}
	if p.WithinWindow != 2 || p.FinishedJobs != 3 || p.TotalJobs != 4 {
		t.Errorf("counts = %+v", p)
	}
	if c := p.Candidates[0]; c.Age != "8d8h" || c.AgeSeconds != 200*3600 {
		t.Errorf("age = %s / %d", c.Age, c.AgeSeconds)
	}
}

func TestPlanKeepLastPerCronJob(t *testing.T) {
	jobs := []audit.Job{
		cronJobAgo("a-1", "a", 500*time.Hour),
		cronJobAgo("a-2", "a", 400*time.Hour),
		cronJobAgo("a-3", "a", 300*time.Hour),
		cronJobAgo("a-4", "a", 200*time.Hour),
		cronJobAgo("b-1", "b", 500*time.Hour),
		finishedAgo("loose", "batch", 500*time.Hour), // no owner: window only
	}
	p := PlanReap(jobs, Options{OlderThan: 168 * time.Hour, KeepLast: 2, Now: refNow})
	if got, want := names(p), "batch/a-1,batch/a-2,batch/loose"; got != want {
		t.Fatalf("candidates = %s, want %s", got, want)
	}
	if p.KeptLast != 3 {
		t.Errorf("keptLast = %d, want 3 (a-3, a-4, b-1)", p.KeptLast)
	}
	if p.Candidates[0].Owner != "CronJob/a" {
		t.Errorf("owner = %q", p.Candidates[0].Owner)
	}
}

func TestPlanKeepLastZeroReapsAllOld(t *testing.T) {
	jobs := []audit.Job{cronJobAgo("a-1", "a", 500*time.Hour), cronJobAgo("a-2", "a", 400*time.Hour)}
	p := PlanReap(jobs, Options{OlderThan: time.Hour, KeepLast: 0, Now: refNow})
	if len(p.Candidates) != 2 {
		t.Fatalf("candidates = %s", names(p))
	}
}

func TestPlanKeepLastSameNameOtherNamespace(t *testing.T) {
	// CronJobs are namespaced: a CronJob "a" in two namespaces is two owners.
	j1 := cronJobAgo("a-1", "a", 500*time.Hour)
	j2 := cronJobAgo("a-1", "a", 500*time.Hour)
	j2.Namespace, j2.UID = "other", "uid-other"
	p := PlanReap([]audit.Job{j1, j2}, Options{OlderThan: time.Hour, KeepLast: 1, Now: refNow})
	if len(p.Candidates) != 0 || p.KeptLast != 2 {
		t.Fatalf("candidates = %s, keptLast = %d", names(p), p.KeptLast)
	}
}

func TestPlanUndatedJobsKeptAndNeverReaped(t *testing.T) {
	undated := cronJobAgo("a-undated", "a", 0)
	undated.CompletionTime = nil
	jobs := []audit.Job{undated, cronJobAgo("a-1", "a", 500*time.Hour), cronJobAgo("a-2", "a", 400*time.Hour)}
	p := PlanReap(jobs, Options{OlderThan: time.Hour, KeepLast: 1, Now: refNow})
	if got := names(p); got != "batch/a-1,batch/a-2" {
		t.Fatalf("candidates = %s", got)
	}
	loose := finishedAgo("loose", "app", 0)
	loose.CompletionTime = nil
	p = PlanReap([]audit.Job{loose}, Options{OlderThan: time.Hour, Now: refNow})
	if len(p.Candidates) != 0 || p.UnknownAge != 1 {
		t.Fatalf("undated job reaped: %+v", p)
	}
}

func TestPlanSkipsOtherControllers(t *testing.T) {
	j := finishedAgo("wf-step", "app", 500*time.Hour)
	j.OtherOwner = "Workflow/wf"
	p := PlanReap([]audit.Job{j}, Options{OlderThan: time.Hour, Now: refNow})
	if len(p.Candidates) != 0 || p.OtherOwner != 1 {
		t.Fatalf("job of another controller reaped: %+v", p)
	}
}

func TestPlanSelectorAndExclude(t *testing.T) {
	a := finishedAgo("keep-me", "prod", 500*time.Hour)
	a.Labels = map[string]string{"team": "data"}
	b := finishedAgo("nightly-1", "batch", 500*time.Hour)
	b.Labels = map[string]string{"team": "data"}
	c := finishedAgo("other-team", "batch", 500*time.Hour)
	c.Labels = map[string]string{"team": "web"}
	d := finishedAgo("report", "batch", 500*time.Hour)
	d.Labels = map[string]string{"team": "data"}

	sel, err := labels.Parse("team=data")
	if err != nil {
		t.Fatal(err)
	}
	p := PlanReap([]audit.Job{a, b, c, d}, Options{
		OlderThan: time.Hour, Now: refNow, Selector: sel,
		Exclude: []string{"prod/*", "nightly-*"},
	})
	if got := names(p); got != "batch/report" {
		t.Fatalf("candidates = %s, want batch/report", got)
	}
	if p.NotSelected != 1 || p.Excluded != 2 {
		t.Errorf("notSelected = %d, excluded = %d", p.NotSelected, p.Excluded)
	}
}

func TestValidateExclude(t *testing.T) {
	for _, ok := range []string{"job", "ns/job", "*/nightly-*", "batch/*"} {
		if err := ValidateExclude([]string{ok}); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ns/", "a/b/c", "[", "ns/["} {
		if err := ValidateExclude([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

type fakeDeleter struct {
	calls []string
	fail  map[string]error
}

func (f *fakeDeleter) DeleteJob(_ context.Context, ns, name, uid string) error {
	f.calls = append(f.calls, ns+"/"+name+"@"+uid)
	return f.fail[name]
}

func TestExecuteCountsAndContinues(t *testing.T) {
	plan := PlanReap([]audit.Job{
		finishedAgo("a", "app", 500*time.Hour),
		finishedAgo("b", "app", 500*time.Hour),
		finishedAgo("c", "batch", 500*time.Hour),
		finishedAgo("d", "batch", 500*time.Hour),
	}, Options{OlderThan: time.Hour, Now: refNow})
	d := &fakeDeleter{fail: map[string]error{
		"b": errors.New("conflict"),
		"c": fmt.Errorf("delete job batch/c: %w", ErrGone),
	}}
	m := NewMetrics()
	res := Execute(context.Background(), plan, d, m)
	if res.Deleted != 2 || res.Gone != 1 || len(res.Errors) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(d.calls) != 4 || d.calls[0] != "app/a@uid-app-a" {
		t.Errorf("calls = %v", d.calls)
	}

	m.ObserveRun(len(plan.Candidates), true, 1700000000)
	var buf bytes.Buffer
	if _, err := m.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`k8s_job_ttl_audit_reaped_jobs_total{namespace="app"} 1`,
		`k8s_job_ttl_audit_reaped_jobs_total{namespace="batch"} 1`,
		"k8s_job_ttl_audit_reap_delete_errors_total 1",
		"k8s_job_ttl_audit_reap_runs_total 1",
		"k8s_job_ttl_audit_reap_run_errors_total 1",
		"k8s_job_ttl_audit_reap_candidates 4",
		"# TYPE k8s_job_ttl_audit_reaped_jobs_total counter",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
}

func TestExecuteStopsOnCancel(t *testing.T) {
	plan := PlanReap([]audit.Job{finishedAgo("a", "app", 500*time.Hour)}, Options{OlderThan: time.Hour, Now: refNow})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &fakeDeleter{}
	res := Execute(ctx, plan, d, nil)
	if len(d.calls) != 0 || len(res.Errors) != 1 {
		t.Fatalf("calls = %v, result = %+v", d.calls, res)
	}
}

func TestRenderTable(t *testing.T) {
	plan := PlanReap([]audit.Job{cronJobAgo("a-1", "a", 200*time.Hour)}, Options{OlderThan: 168 * time.Hour, Now: refNow})
	var buf bytes.Buffer
	if err := RenderTable(&buf, plan, true); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"NAMESPACE  JOB  OWNER      STATUS    COMPLETED             AGE",
		"batch      a-1  CronJob/a  Complete  2026-09-02T04:00:00Z  8d8h",
		"Would delete (dry run): 1 of 1 finished (total 1).",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}
