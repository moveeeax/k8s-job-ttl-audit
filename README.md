# k8s-job-ttl-audit

[![ci](https://github.com/moveeeax/k8s-job-ttl-audit/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/k8s-job-ttl-audit/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8?logo=go)](https://go.dev/)
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
- Reports each stale Job's completion age, TTL, owning `CronJob`, and count of
  **leftover Pods** it still holds.
- Summarizes total stale Jobs and leftover Pods, with a per-namespace breakdown.
- Outputs a **table** (default) or **JSON** (`-o json`), and gates CI with
  `--fail-on stale`.
- Runs live against a cluster **or fully offline** from a JSON/YAML dump.

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
NAMESPACE  JOB                   STATUS    REASON       AGE     TTL      PODS  OWNER
app        db-migrate-v37        Complete  ttl-too-big  16h51m  604800s  1     -
batch      nightly-backup-28401  Complete  no-ttl       21d9h   none     1     CronJob/nightly-backup

Stale Jobs: 2   Leftover Pods: 2   (finished 3 / total 4)

Per namespace:
  NAMESPACE  STALE  PODS
  app        1      1
  batch      1      1
```

### Offline mode

Feed it any `kubectl get jobs,pods -o json` dump — no cluster required. This is also
how the audit logic is unit-tested:

```sh
kubectl get jobs,pods -A -o json > dump.json
k8s-job-ttl-audit scan --input dump.json --max-ttl 24h --fail-on stale
```

### Flags

| Flag | Description |
| --- | --- |
| `-A, --all-namespaces` | Scan all namespaces. |
| `-n, --namespace` | Namespace to scan (default: current context namespace). |
| `--max-ttl` | Flag finished Jobs whose TTL exceeds this (e.g. `24h`). `0` flags only a missing TTL. |
| `-o, --output` | `table` (default) or `json`. |
| `--input` | Read Jobs+Pods from a JSON/YAML dump instead of a live cluster. |
| `--fail-on stale` | Exit non-zero when any stale Job exists (for CI). |
| `--kubeconfig`, `--context` | Standard kubeconfig selection. |

## How it works

The classifier is a **pure function** over lists of Jobs and Pods:

1. Skip any Job that is not finished (no `Complete`/`Failed` condition).
2. A finished Job with no `ttlSecondsAfterFinished` → **stale** (`no-ttl`).
3. A finished Job whose TTL is strictly greater than `--max-ttl` → **stale**
   (`ttl-too-big`). TTL exactly at the threshold is fine.
4. Leftover Pods are counted by matching each Pod's owner `Job` UID.

Because classification never touches the network, the whole core is exercised
offline against fixtures with no API server — that's the `--input` mode too.
Live cluster access (client-go) only adapts real Job/Pod objects into those plain
structs and hands them to the same function.

## License

[MIT](LICENSE)
