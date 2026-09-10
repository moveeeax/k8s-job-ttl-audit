package audit

import (
	"testing"
	"time"
)

const sampleDump = `{
  "apiVersion": "v1",
  "kind": "List",
  "items": [
    {
      "apiVersion": "batch/v1",
      "kind": "Job",
      "metadata": {"name": "backup", "namespace": "batch", "uid": "job-1",
        "ownerReferences": [{"kind": "CronJob", "name": "nightly", "uid": "cj-1"}]},
      "spec": {},
      "status": {
        "completionTime": "2026-09-07T08:00:00Z",
        "conditions": [{"type": "Complete", "status": "True"}]
      }
    },
    {
      "apiVersion": "batch/v1",
      "kind": "Job",
      "metadata": {"name": "migrate", "namespace": "batch", "uid": "job-2"},
      "spec": {"ttlSecondsAfterFinished": 600},
      "status": {
        "completionTime": "2026-09-10T11:00:00Z",
        "conditions": [{"type": "Complete", "status": "True"}]
      }
    },
    {
      "apiVersion": "batch/v1",
      "kind": "Job",
      "metadata": {"name": "running", "namespace": "batch", "uid": "job-3"},
      "spec": {},
      "status": {"active": 1}
    },
    {
      "apiVersion": "v1",
      "kind": "Pod",
      "metadata": {"name": "backup-abc", "namespace": "batch",
        "ownerReferences": [{"kind": "Job", "name": "backup", "uid": "job-1"}]}
    },
    {
      "apiVersion": "v1",
      "kind": "Pod",
      "metadata": {"name": "backup-def", "namespace": "batch",
        "ownerReferences": [{"kind": "Job", "name": "backup", "uid": "job-1"}]}
    }
  ]
}`

func TestParseInputJSON(t *testing.T) {
	jobs, pods, err := ParseInput([]byte(sampleDump))
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("jobs = %d, want 3", len(jobs))
	}
	if len(pods) != 2 {
		t.Fatalf("pods = %d, want 2", len(pods))
	}

	var backup Job
	for _, j := range jobs {
		if j.Name == "backup" {
			backup = j
		}
	}
	if !backup.Finished || backup.FinishedReason != "Complete" {
		t.Errorf("backup not finished-complete: %+v", backup)
	}
	if backup.OwnerKind != "CronJob" || backup.OwnerName != "nightly" {
		t.Errorf("owner not parsed: %+v", backup)
	}
	if backup.CompletionTime == nil {
		t.Fatal("completionTime not parsed")
	}

	// End-to-end classification over the parsed dump.
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	rep := Classify(jobs, pods, Options{Now: now, MaxTTL: 24 * time.Hour})
	if rep.StaleJobs != 1 {
		t.Fatalf("stale = %d, want 1 (only backup has no TTL)", rep.StaleJobs)
	}
	if rep.Findings[0].Name != "backup" {
		t.Errorf("wrong job stale: %s", rep.Findings[0].Name)
	}
	if rep.Findings[0].LeftoverPods != 2 {
		t.Errorf("backup leftover pods = %d, want 2", rep.Findings[0].LeftoverPods)
	}
}

func TestParseInputYAML(t *testing.T) {
	const y = `
items:
  - kind: Job
    metadata: {name: j1, namespace: default, uid: u1}
    status:
      completionTime: "2026-09-01T00:00:00Z"
      conditions:
        - {type: Complete, status: "True"}
`
	jobs, _, err := ParseInput([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || !jobs[0].Finished {
		t.Fatalf("yaml parse failed: %+v", jobs)
	}
}

func TestParseInputEmpty(t *testing.T) {
	if _, _, err := ParseInput([]byte(`{"items":[]}`)); err == nil {
		t.Fatal("expected error on empty items")
	}
}
