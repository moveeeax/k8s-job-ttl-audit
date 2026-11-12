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
	inv, err := ParseInput([]byte(sampleDump))
	if err != nil {
		t.Fatal(err)
	}
	jobs, pods := inv.Jobs, inv.Pods
	if inv.CronJobsListed {
		t.Error("CronJobsListed = true for a dump without CronJobs")
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
	inv, err := ParseInput([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	jobs := inv.Jobs
	if len(jobs) != 1 || !jobs[0].Finished {
		t.Fatalf("yaml parse failed: %+v", jobs)
	}
}

func TestParseInputEmpty(t *testing.T) {
	if _, err := ParseInput([]byte(`{"items":[]}`)); err == nil {
		t.Fatal("expected error on empty items")
	}
}

const cronDump = `{
  "kind": "List",
  "items": [
    {
      "kind": "CronJob",
      "metadata": {"name": "nightly", "namespace": "batch", "uid": "cj-1"},
      "spec": {"successfulJobsHistoryLimit": 7}
    },
    {
      "kind": "Job",
      "metadata": {"name": "nightly-1", "namespace": "batch", "uid": "job-1",
        "labels": {"team": "data"},
        "ownerReferences": [{"kind": "CronJob", "name": "nightly", "uid": "cj-1", "controller": true}]},
      "spec": {},
      "status": {"conditions": [{"type": "Failed", "status": "True", "lastTransitionTime": "2026-09-01T00:00:00Z"}]}
    },
    {
      "kind": "Job",
      "metadata": {"name": "op-run", "namespace": "batch", "uid": "job-2",
        "ownerReferences": [{"kind": "Workflow", "name": "wf", "uid": "wf-1", "controller": true}]},
      "spec": {},
      "status": {"conditions": [{"type": "Complete", "status": "True"}]}
    }
  ]
}`

func TestParseInputCronJobsAndOwners(t *testing.T) {
	inv, err := ParseInput([]byte(cronDump))
	if err != nil {
		t.Fatal(err)
	}
	if !inv.CronJobsListed || len(inv.CronJobs) != 1 {
		t.Fatalf("cronjobs not parsed: listed=%v %+v", inv.CronJobsListed, inv.CronJobs)
	}
	cj := inv.CronJobs[0]
	if cj.Name != "nightly" || cj.Namespace != "batch" {
		t.Errorf("cronjob identity = %s/%s", cj.Namespace, cj.Name)
	}
	if cj.SuccessfulJobsHistoryLimit == nil || *cj.SuccessfulJobsHistoryLimit != 7 {
		t.Errorf("successful limit = %v, want 7", cj.SuccessfulJobsHistoryLimit)
	}
	if cj.FailedJobsHistoryLimit != nil {
		t.Errorf("failed limit = %v, want nil (API default)", *cj.FailedJobsHistoryLimit)
	}

	j := inv.Jobs[0]
	if j.Labels["team"] != "data" {
		t.Errorf("labels = %v", j.Labels)
	}
	want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if j.CompletionTime == nil || !j.CompletionTime.Equal(want) {
		t.Errorf("failed job completion fallback = %v, want %v", j.CompletionTime, want)
	}
	if j.OwnerUID != "cj-1" || cj.UID != "cj-1" {
		t.Errorf("owner UID = %q, cronjob UID = %q, want cj-1", j.OwnerUID, cj.UID)
	}
	if j.OtherOwner != "" {
		t.Errorf("CronJob-owned job got OtherOwner %q", j.OtherOwner)
	}
	if got := inv.Jobs[1].OtherOwner; got != "Workflow/wf" {
		t.Errorf("OtherOwner = %q, want Workflow/wf", got)
	}
}
