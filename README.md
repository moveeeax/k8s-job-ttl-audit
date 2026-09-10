# k8s-job-ttl-audit
CLI that audits Kubernetes Jobs and CronJob templates for a missing or too-long ttlSecondsAfterFinished, so finished Jobs and their Pods stop piling up in etcd. Reports each Job's completion age, TTL setting, and leftover Pods, with table/JSON output and a --fail-on gate for CI.
