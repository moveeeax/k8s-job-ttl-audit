# k8s-job-ttl-audit

[![ci](https://github.com/moveeeax/k8s-job-ttl-audit/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/k8s-job-ttl-audit/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

Audit Kubernetes **Jobs** for a missing or too-long `ttlSecondsAfterFinished`, so
finished Jobs and their Pods stop piling up in etcd.

Kubernetes only garbage-collects a finished Job when `spec.ttlSecondsAfterFinished`
is set. Teams forget it, so completed Jobs accumulate for months — each one keeping
its Pods, events, and etcd entries. There is no built-in view that answers *"which
Jobs have completed but will never be collected, and how many stale Pods are they
holding?"* This tool is that view.

## What it does

- Lists Jobs in one namespace or across the whole cluster.
- Flags a **finished** Job (`Complete` / `Failed`) as **stale** when it has:
  - **no TTL** (`ttlSecondsAfterFinished` unset), or
  - a TTL **greater than `--max-ttl`** (a threshold you consider sane).
- Accounts for **CronJob history limits**: a TTL-less Job created by a CronJob
  is already deleted by the CronJob controller once it falls out of
  `successfulJobsHistoryLimit` / `failedJobsHistoryLimit` (defaults 3 / 1), so it
  is flagged only when that limit is above `--max-history`, or as
  `orphaned-cronjob-job` when the owning CronJob no longer exists.
- Reports each stale Job's completion age, TTL, owning `CronJob`, and count of
  **leftover Pods** it still holds.
- Summarizes total stale Jobs and leftover Pods, with a per-namespace breakdown.
- Outputs a **table** (default) or **JSON** (`-o json`), and gates CI with
  `--fail-on stale`.
- Runs live against a cluster **or fully offline** from a JSON/YAML dump.
- `reap` deletes finished Jobs older than a retention window while keeping the
  newest N per CronJob (dry run by default, see [Reaping](#reaping)).

A running or not-yet-finished Job is **never** flagged.

## Install

```sh
go install github.com/moveeeax/k8s-job-ttl-audit/cmd/k8s-job-ttl-audit@latest
```

Or build from source:

```sh
git clone https://github.com/moveeeax/k8s-job-ttl-audit
cd k8s-job-ttl-audit
go build -o k8s-job-ttl-audit ./cmd/k8s-job-ttl-audit
```

## Usage

```sh
# Audit every namespace, flag finished Jobs with no TTL
k8s-job-ttl-audit scan --all-namespaces

# One namespace, JSON for a dashboard, 24h max TTL
k8s-job-ttl-audit scan -n batch --max-ttl 24h --output json

# Gate CI: fail if any stale Job exists
k8s-job-ttl-audit scan --all-namespaces --fail-on stale
```

Example against the bundled fixture:

```console
$ k8s-job-ttl-audit scan --input examples/jobs-dump.json --max-ttl 24h
NAMESPACE  JOB                  STATUS    REASON                AGE     TTL      PODS  OWNER                  HISTORY
app        db-migrate-v37       Complete  ttl-too-big           16h51m  604800s  1     -                      -
app        weekly-report-27990  Complete  orphaned-cronjob-job  46d5h   none     0     CronJob/weekly-report  -
batch      hourly-sync-29299    Complete  no-ttl                1h20m   none     0     CronJob/hourly-sync    100

Stale Jobs: 3   Leftover Pods: 1   (finished 5 / total 6, 1 bounded by CronJob history <= 10)

Per namespace:
  NAMESPACE  STALE  PODS
  app        2      1
  batch      1      0
```

`nightly-backup-28401` has no TTL either, but its CronJob keeps at most 3
successful Jobs, so it is counted as bounded instead of flagged. `HISTORY` is the
owner's history limit for the Job's state (successful for `Complete`, failed for
`Failed`).

### Offline mode

Feed it any `kubectl get jobs,pods,cronjobs -o json` dump — no cluster required. This
is also how the audit logic is unit-tested:

```sh
kubectl get jobs,pods,cronjobs -A -o json > dump.json
k8s-job-ttl-audit scan --input dump.json --max-ttl 24h --fail-on stale
```

A dump without any CronJob (for example an older `kubectl get jobs,pods` dump)
cannot tell a deleted CronJob from one that was not dumped, so history limits are
skipped and every TTL-less Job is reported as `no-ttl`, as before.

### Flags

| Flag | Description |
| --- | --- |
| `-A, --all-namespaces` | Scan all namespaces. |
| `-n, --namespace` | Namespace to scan (default: current context namespace). |
| `--max-ttl` | Flag finished Jobs whose TTL exceeds this (e.g. `24h`). `0` flags only a missing TTL. |
| `--max-history` | Do not flag a TTL-less CronJob Job when the CronJob's history limit for its state is at most this (default `10`). |
| `-o, --output` | `table` (default) or `json`. |
| `--input` | Read Jobs, Pods and CronJobs from a JSON/YAML dump instead of a live cluster. |
| `--fail-on stale` | Exit non-zero when any stale Job exists (for CI). |
| `--kubeconfig`, `--context` | Standard kubeconfig selection. |

Live scans need `list` on `jobs`, `pods` and `cronjobs`. Without RBAC to list
CronJobs the scan prints a warning and runs without history limits.

## Reaping

`reap` deletes finished (`Complete` / `Failed`) Jobs that finished longer ago than
`--older-than`, keeping the newest `--keep-last` finished Jobs of every owning
CronJob so recent history stays readable after a sweep. Each Job is deleted with
`propagationPolicy: Background`, so the garbage collector removes its Pods too,
and with a UID precondition, so a Job recreated under the same name since the
listing is never touched.

It is a **dry run by default**: it only prints what it would delete. Pass
`--dry-run=false` to delete.

```sh
# What would go: older than a week, keep the last 5 per CronJob
k8s-job-ttl-audit reap --all-namespaces --older-than 168h --keep-last 5

# Delete, only Jobs labelled team=data, never anything in prod or named nightly-*
k8s-job-ttl-audit reap -A -l team=data --exclude 'prod/*' --exclude 'nightly-*' --dry-run=false

# Run in-cluster every hour with Prometheus metrics on :9090/metrics
k8s-job-ttl-audit reap -A --dry-run=false --interval 1h --metrics-addr :9090
```

Against the bundled fixture, as of 2026-09-10 11:21 UTC:

```console
$ k8s-job-ttl-audit reap --input examples/jobs-dump.json --older-than 168h --keep-last 0
NAMESPACE  JOB                   OWNER                   STATUS    COMPLETED             AGE
app        weekly-report-27990   CronJob/weekly-report   Complete  2026-07-26T06:00:00Z  46d5h
batch      nightly-backup-28401  CronJob/nightly-backup  Complete  2026-08-20T02:00:00Z  21d9h

Would delete (dry run): 2 of 5 finished (total 6). Kept: 0 newest per CronJob, 3 within 7d, 0 other owner, 0 unknown age, 0 not selected, 0 excluded.
```

Never reaped, whatever the flags:

- running or not-yet-finished Jobs;
- Jobs whose controller owner is not a CronJob (an operator or workflow engine
  that manages its own Jobs);
- Jobs with no completion time to age them by.

`--keep-last` groups Jobs by the owning CronJob's namespace and name, also when
that CronJob is gone. Jobs without a CronJob owner are not grouped: only the
retention window applies to them. `--input` works for planning only
(`--dry-run`, no `--interval`).

| Flag | Description |
| --- | --- |
| `--older-than` | Retention window: reap Jobs that finished strictly longer ago than this (default `168h`). |
| `--keep-last` | Never reap the newest N finished Jobs of each CronJob (default `5`; `0` keeps none). |
| `-l, --selector` | Only reap Jobs whose labels match this selector. |
| `--exclude` | Never reap these Jobs: `name` or `namespace/name`, globs allowed, repeatable. |
| `--dry-run` | Only list what would be deleted (default `true`). `--dry-run=false` deletes. |
| `-o, --output` | `table` (default) or `json`. |
| `--interval` | Repeat the sweep at this interval until SIGINT/SIGTERM; `0` (default) runs once. |
| `--metrics-addr` | Serve Prometheus metrics at `/metrics` on this address; needs `--interval`. |
| `-A`, `-n`, `--input`, `--kubeconfig`, `--context` | Same as `scan`. |

A single run exits 2 when any deletion fails; in loop mode errors are logged and
the next sweep runs on schedule. Deleting needs `delete` on `jobs` in addition to
the `list` permissions above.

Metrics:

| Metric | Type | Meaning |
| --- | --- | --- |
| `k8s_job_ttl_audit_reap_runs_total` | counter | Sweeps run. |
| `k8s_job_ttl_audit_reap_run_errors_total` | counter | Sweeps that failed to list or had delete errors. |
| `k8s_job_ttl_audit_reaped_jobs_total{namespace}` | counter | Jobs deleted. |
| `k8s_job_ttl_audit_reap_delete_errors_total` | counter | Deletions that failed. |
| `k8s_job_ttl_audit_reap_candidates` | gauge | Jobs eligible in the last sweep. |
| `k8s_job_ttl_audit_reap_last_success_timestamp_seconds` | gauge | Unix time of the last sweep without errors. |

## How it works

The classifier is a **pure function** over lists of Jobs and Pods:

1. Skip any Job that is not finished (no `Complete`/`Failed` condition).
2. A finished Job with no `ttlSecondsAfterFinished`:
   - owned by a CronJob that is not in the listing → **stale**
     (`orphaned-cronjob-job`): no history limit will ever clean it up;
   - owned by a CronJob whose history limit for the Job's state is at most
     `--max-history` → not stale, counted as bounded;
   - otherwise → **stale** (`no-ttl`).
3. A finished Job whose TTL is strictly greater than `--max-ttl` → **stale**
   (`ttl-too-big`). TTL exactly at the threshold is fine.
4. Leftover Pods are counted by matching each Pod's owner `Job` UID.

A Job's owner is matched by namespace and name, and by UID when both sides carry
one. The CronJob controller finds the Jobs it cleans up by the owner reference
UID, so after a CronJob is deleted and recreated under the same name its old Jobs
are reported as `orphaned-cronjob-job`: nothing will delete them.

Because classification never touches the network, the whole core is exercised
offline against fixtures with no API server — that's the `--input` mode too.
Live cluster access (client-go) only adapts real Job/Pod objects into those plain
structs and hands them to the same function.

## License

[MIT](LICENSE)
