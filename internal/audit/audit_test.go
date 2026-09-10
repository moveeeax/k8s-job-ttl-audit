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
