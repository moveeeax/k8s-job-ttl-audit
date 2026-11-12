package audit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func ptr32(v int32) *int32 { return &v }

var refNow = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func completedAt(d time.Duration) *time.Time {
	t := refNow.Add(-d)
	return &t
}

func TestRunningJobNeverFlagged(t *testing.T) {
	jobs := []Job{
		{Name: "still-running", Namespace: "default", UID: "u1", Finished: false},
	}
	rep := Classify(jobs, nil, Options{Now: refNow})
	if rep.StaleJobs != 0 {
		t.Fatalf("running job flagged: %d stale", rep.StaleJobs)
	}
	if rep.FinishedJobs != 0 {
		t.Fatalf("running job counted as finished")
	}
}

func TestFinishedNoTTLIsStale(t *testing.T) {
	jobs := []Job{
		{
			Name: "nightly-backup", Namespace: "batch", UID: "u1",
			Finished: true, FinishedReason: "Complete",
			CompletionTime: completedAt(76 * time.Hour), // 3d4h
			TTLSeconds:     nil,
		},
	}
	rep := Classify(jobs, nil, Options{Now: refNow})
	if rep.StaleJobs != 1 {
		t.Fatalf("expected 1 stale, got %d", rep.StaleJobs)
	}
	f := rep.Findings[0]
	if f.Reason != ReasonNoTTL {
		t.Errorf("reason = %q, want %q", f.Reason, ReasonNoTTL)
	}
	if f.CompletionAge != "3d4h" {
		t.Errorf("age = %q, want 3d4h", f.CompletionAge)
	}
	if f.Status != "Complete" {
		t.Errorf("status = %q", f.Status)
	}
}

func TestTTLOverThresholdFlagged(t *testing.T) {
	jobs := []Job{
		{Name: "too-big", Namespace: "batch", UID: "u1", Finished: true,
			FinishedReason: "Complete", CompletionTime: completedAt(time.Hour),
			TTLSeconds: ptr32(int32((48 * time.Hour).Seconds()))},
	}
	rep := Classify(jobs, nil, Options{Now: refNow, MaxTTL: 24 * time.Hour})
	if rep.StaleJobs != 1 {
		t.Fatalf("expected 1 stale, got %d", rep.StaleJobs)
	}
	if rep.Findings[0].Reason != ReasonTTLTooBig {
		t.Errorf("reason = %q, want %q", rep.Findings[0].Reason, ReasonTTLTooBig)
	}
}

func TestTTLWithinThresholdNotFlagged(t *testing.T) {
	jobs := []Job{
		{Name: "ok", Namespace: "batch", UID: "u1", Finished: true,
			FinishedReason: "Complete", CompletionTime: completedAt(time.Hour),
			TTLSeconds: ptr32(int32((12 * time.Hour).Seconds()))},
	}
	rep := Classify(jobs, nil, Options{Now: refNow, MaxTTL: 24 * time.Hour})
	if rep.StaleJobs != 0 {
		t.Fatalf("job within threshold flagged: %d", rep.StaleJobs)
	}
	if rep.FinishedJobs != 1 {
		t.Fatalf("finished not counted: %d", rep.FinishedJobs)
	}
}

func TestTTLExactlyAtThresholdNotFlagged(t *testing.T) {
	// Boundary: TTL == max-ttl is sane; only strictly greater is stale.
	jobs := []Job{
		{Name: "edge", Namespace: "batch", UID: "u1", Finished: true,
			FinishedReason: "Complete", CompletionTime: completedAt(time.Hour),
			TTLSeconds: ptr32(int32((24 * time.Hour).Seconds()))},
	}
	rep := Classify(jobs, nil, Options{Now: refNow, MaxTTL: 24 * time.Hour})
	if rep.StaleJobs != 0 {
		t.Fatalf("TTL at threshold flagged: %d", rep.StaleJobs)
	}
}

func TestMaxTTLZeroOnlyFlagsMissing(t *testing.T) {
	jobs := []Job{
		{Name: "has-huge-ttl", Namespace: "batch", UID: "u1", Finished: true,
			FinishedReason: "Complete", CompletionTime: completedAt(time.Hour),
			TTLSeconds: ptr32(999999)},
		{Name: "no-ttl", Namespace: "batch", UID: "u2", Finished: true,
			FinishedReason: "Failed", CompletionTime: completedAt(time.Hour)},
	}
	rep := Classify(jobs, nil, Options{Now: refNow, MaxTTL: 0})
	if rep.StaleJobs != 1 {
		t.Fatalf("expected only the missing-TTL job stale, got %d", rep.StaleJobs)
	}
	if rep.Findings[0].Name != "no-ttl" {
		t.Errorf("wrong job flagged: %s", rep.Findings[0].Name)
	}
}

func TestLeftoverPodsCounted(t *testing.T) {
	jobs := []Job{
		{Name: "j", Namespace: "batch", UID: "job-uid", Finished: true,
			FinishedReason: "Complete", CompletionTime: completedAt(time.Hour)},
	}
	pods := []Pod{
		{Name: "j-a", Namespace: "batch", OwnerJobUID: "job-uid"},
		{Name: "j-b", Namespace: "batch", OwnerJobUID: "job-uid"},
		{Name: "unrelated", Namespace: "batch", OwnerJobUID: "other-uid"},
	}
	rep := Classify(jobs, pods, Options{Now: refNow})
	if rep.Findings[0].LeftoverPods != 2 {
		t.Fatalf("leftover pods = %d, want 2", rep.Findings[0].LeftoverPods)
	}
	if rep.LeftoverPods != 2 {
		t.Fatalf("total leftover pods = %d, want 2", rep.LeftoverPods)
	}
}

func TestPerNamespaceSummaryAndSorting(t *testing.T) {
	jobs := []Job{
		{Name: "b", Namespace: "zeta", UID: "u1", Finished: true, FinishedReason: "Complete", CompletionTime: completedAt(time.Hour)},
		{Name: "a", Namespace: "alpha", UID: "u2", Finished: true, FinishedReason: "Complete", CompletionTime: completedAt(time.Hour)},
		{Name: "c", Namespace: "alpha", UID: "u3", Finished: true, FinishedReason: "Failed", CompletionTime: completedAt(time.Hour)},
	}
	rep := Classify(jobs, nil, Options{Now: refNow})
	if len(rep.ByNamespace) != 2 {
		t.Fatalf("namespaces = %d, want 2", len(rep.ByNamespace))
	}
	if rep.ByNamespace[0].Namespace != "alpha" || rep.ByNamespace[1].Namespace != "zeta" {
		t.Errorf("namespaces not sorted: %+v", rep.ByNamespace)
	}
	if rep.ByNamespace[0].StaleJobs != 2 {
		t.Errorf("alpha stale = %d, want 2", rep.ByNamespace[0].StaleJobs)
	}
	// Findings sorted by (namespace, name): alpha/a, alpha/c, zeta/b.
	got := []string{}
	for _, f := range rep.Findings {
		got = append(got, f.Namespace+"/"+f.Name)
	}
	want := "alpha/a,alpha/c,zeta/b"
	if strings.Join(got, ",") != want {
		t.Errorf("finding order = %v, want %s", got, want)
	}
}

func TestJSONOutputStable(t *testing.T) {
	jobs := []Job{
		{Name: "j", Namespace: "batch", UID: "u1", Finished: true,
			FinishedReason: "Complete", CompletionTime: completedAt(time.Hour)},
	}
	rep := Classify(jobs, nil, Options{Now: refNow})
	var buf bytes.Buffer
	if err := RenderJSON(&buf, rep); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("json round-trip failed: %v", err)
	}
	if back.StaleJobs != 1 {
		t.Errorf("round-tripped staleJobs = %d", back.StaleJobs)
	}
}

func TestCronJobOwnerRecorded(t *testing.T) {
	jobs := []Job{
		{Name: "nightly-1", Namespace: "batch", UID: "u1", Finished: true,
			FinishedReason: "Complete", CompletionTime: completedAt(time.Hour),
			OwnerKind: "CronJob", OwnerName: "nightly"},
	}
	rep := Classify(jobs, nil, Options{Now: refNow})
	if rep.Findings[0].Owner != "CronJob/nightly" {
		t.Errorf("owner = %q, want CronJob/nightly", rep.Findings[0].Owner)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                            "0s",
		45 * time.Second:             "45s",
		90 * time.Second:             "1m30s",
		2*time.Hour + 15*time.Minute: "2h15m",
		76 * time.Hour:               "3d4h",
		48 * time.Hour:               "2d",
	}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func cronOwned(name, owner, reason string) Job {
	return Job{Name: name, Namespace: "batch", UID: name, Finished: true,
		FinishedReason: reason, CompletionTime: completedAt(time.Hour),
		OwnerKind: "CronJob", OwnerName: owner}
}

func TestCronJobHistoryLimitBoundsNoTTL(t *testing.T) {
	jobs := []Job{
		cronOwned("default-limits-1", "default-limits", "Complete"),
		cronOwned("default-limits-2", "default-limits", "Failed"),
		cronOwned("big-1", "big", "Complete"),
		cronOwned("big-failed-1", "big", "Failed"),
		cronOwned("gone-1", "gone", "Complete"),
	}
	crons := []CronJob{
		{Name: "default-limits", Namespace: "batch"}, // API defaults 3/1
		{Name: "big", Namespace: "batch", SuccessfulJobsHistoryLimit: ptr32(50), FailedJobsHistoryLimit: ptr32(2)},
		// Same name in another namespace must not count as the owner.
		{Name: "gone", Namespace: "other"},
	}
	rep := Classify(jobs, nil, Options{Now: refNow, CronJobs: crons, CronJobsListed: true, MaxHistory: 10})

	got := map[string]Finding{}
	for _, f := range rep.Findings {
		got[f.Name] = f
	}
	if len(got) != 2 {
		t.Fatalf("findings = %+v, want big-1 and gone-1", rep.Findings)
	}
	if f := got["big-1"]; f.Reason != ReasonNoTTL || f.HistoryLimit == nil || *f.HistoryLimit != 50 {
		t.Errorf("big-1 = %+v, want no-ttl with history 50", f)
	}
	if f := got["gone-1"]; f.Reason != ReasonOrphanedCronJob || f.HistoryLimit != nil {
		t.Errorf("gone-1 = %+v, want orphaned-cronjob-job without history", f)
	}
	// default-limits-1 (3), default-limits-2 (1) and big-failed-1 (2) are bounded.
	if rep.BoundedByHistory != 3 {
		t.Errorf("boundedByHistory = %d, want 3", rep.BoundedByHistory)
	}
	if rep.MaxHistory == nil || *rep.MaxHistory != 10 {
		t.Errorf("maxHistory = %v, want 10", rep.MaxHistory)
	}
}

func TestCronJobHistoryLimitAtThresholdBounded(t *testing.T) {
	jobs := []Job{cronOwned("edge-1", "edge", "Complete")}
	crons := []CronJob{{Name: "edge", Namespace: "batch", SuccessfulJobsHistoryLimit: ptr32(10)}}
	rep := Classify(jobs, nil, Options{Now: refNow, CronJobs: crons, CronJobsListed: true, MaxHistory: 10})
	if rep.StaleJobs != 0 {
		t.Fatalf("limit == max-history flagged: %+v", rep.Findings)
	}
	rep = Classify(jobs, nil, Options{Now: refNow, CronJobs: crons, CronJobsListed: true, MaxHistory: 9})
	if rep.StaleJobs != 1 || rep.Findings[0].Reason != ReasonNoTTL {
		t.Fatalf("limit > max-history not flagged no-ttl: %+v", rep.Findings)
	}
}

func TestCronJobsNotListedKeepsNoTTL(t *testing.T) {
	// Without a CronJob listing a missing owner proves nothing: keep no-ttl.
	jobs := []Job{cronOwned("n-1", "nightly", "Complete")}
	rep := Classify(jobs, nil, Options{Now: refNow, MaxHistory: 10})
	if rep.StaleJobs != 1 || rep.Findings[0].Reason != ReasonNoTTL {
		t.Fatalf("want no-ttl, got %+v", rep.Findings)
	}
	if rep.MaxHistory != nil || rep.Findings[0].HistoryLimit != nil {
		t.Errorf("history reported without a CronJob listing: %+v", rep)
	}
}

func TestCronJobHistoryDoesNotHideTTLTooBig(t *testing.T) {
	j := cronOwned("n-1", "nightly", "Complete")
	j.TTLSeconds = ptr32(int32((48 * time.Hour).Seconds()))
	crons := []CronJob{{Name: "nightly", Namespace: "batch"}}
	rep := Classify([]Job{j}, nil, Options{Now: refNow, MaxTTL: 24 * time.Hour,
		CronJobs: crons, CronJobsListed: true, MaxHistory: 10})
	if rep.StaleJobs != 1 || rep.Findings[0].Reason != ReasonTTLTooBig {
		t.Fatalf("want ttl-too-big, got %+v", rep.Findings)
	}
	if rep.BoundedByHistory != 0 {
		t.Errorf("job with a TTL counted as bounded: %d", rep.BoundedByHistory)
	}
}

func TestRenderTableHistoryColumn(t *testing.T) {
	jobs := []Job{cronOwned("big-1", "big", "Complete"), cronOwned("ok-1", "ok", "Complete")}
	crons := []CronJob{
		{Name: "big", Namespace: "batch", SuccessfulJobsHistoryLimit: ptr32(50)},
		{Name: "ok", Namespace: "batch"},
	}
	rep := Classify(jobs, nil, Options{Now: refNow, CronJobs: crons, CronJobsListed: true, MaxHistory: 10})
	var buf bytes.Buffer
	if err := RenderTable(&buf, rep); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	lines := strings.Split(out, "\n")
	if !strings.HasSuffix(strings.TrimSpace(lines[0]), "HISTORY") {
		t.Errorf("header lacks HISTORY: %q", lines[0])
	}
	if !strings.HasSuffix(strings.TrimSpace(lines[1]), "CronJob/big  50") {
		t.Errorf("row lacks history limit: %q", lines[1])
	}
	if !strings.Contains(out, "1 bounded by CronJob history <= 10") {
		t.Errorf("summary lacks bounded count:\n%s", out)
	}
}

func TestRecreatedCronJobOrphansOldJobs(t *testing.T) {
	// Same name, new UID: the controller of the new CronJob does not own,
	// and so never cleans up, Jobs of the old one.
	old := cronOwned("nightly-1", "nightly", "Complete")
	old.OwnerUID = "cj-old"
	cur := cronOwned("nightly-2", "nightly", "Complete")
	cur.OwnerUID = "cj-new"
	crons := []CronJob{{Name: "nightly", Namespace: "batch", UID: "cj-new"}}
	rep := Classify([]Job{old, cur}, nil, Options{Now: refNow, CronJobs: crons, CronJobsListed: true, MaxHistory: 10})
	if rep.StaleJobs != 1 || rep.Findings[0].Name != "nightly-1" || rep.Findings[0].Reason != ReasonOrphanedCronJob {
		t.Fatalf("findings = %+v, want nightly-1 orphaned", rep.Findings)
	}
	if rep.BoundedByHistory != 1 {
		t.Errorf("boundedByHistory = %d, want 1", rep.BoundedByHistory)
	}
}
