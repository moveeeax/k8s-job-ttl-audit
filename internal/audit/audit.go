// Package audit contains the pure classification logic for k8s-job-ttl-audit.
//
// Everything here operates on plain Go structs decoded from Kubernetes Job and
// Pod objects, so the classifier can be exercised entirely offline against
// fixtures with no API server. The cluster package is the only thing that talks
// to a live cluster; it hands these types in.
package audit

import (
	"sort"
	"time"
)

// Job is the slice of a Kubernetes Job we care about for TTL auditing.
type Job struct {
	Name       string
	Namespace  string
	UID        string
	OwnerKind  string // e.g. "CronJob" when the Job was created by one, else ""
	OwnerName  string
	TTLSeconds *int32 // spec.ttlSecondsAfterFinished; nil means unset

	// Terminal state, derived from status conditions.
	Finished       bool
	FinishedReason string // "Complete" or "Failed"
	CompletionTime *time.Time
}

// Pod is the slice of a Kubernetes Pod we care about: which Job owns it.
type Pod struct {
	Name        string
	Namespace   string
	OwnerJobUID string
}

// Reason explains why a Job was classified STALE.
type Reason string

const (
	ReasonNoTTL     Reason = "no-ttl"      // finished Job with no ttlSecondsAfterFinished
	ReasonTTLTooBig Reason = "ttl-too-big" // finished Job whose TTL exceeds --max-ttl
)

// Finding is one stale Job in the report.
type Finding struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace"`
	Status         string `json:"status"` // Complete | Failed
	Reason         Reason `json:"reason"`
	CompletionAge  string `json:"completionAge"` // human duration, e.g. "3d4h"
	CompletionSecs int64  `json:"completionSeconds"`
	TTLSeconds     *int32 `json:"ttlSeconds"`
	LeftoverPods   int    `json:"leftoverPods"`
	Owner          string `json:"owner,omitempty"` // "CronJob/nightly" when known
}

// NamespaceSummary aggregates findings for one namespace.
type NamespaceSummary struct {
	Namespace    string `json:"namespace"`
	StaleJobs    int    `json:"staleJobs"`
	LeftoverPods int    `json:"leftoverPods"`
}

// Report is the full audit result.
type Report struct {
	MaxTTLSeconds *int64             `json:"maxTtlSeconds"` // nil means "any TTL is fine"
	TotalJobs     int                `json:"totalJobs"`
	FinishedJobs  int                `json:"finishedJobs"`
	StaleJobs     int                `json:"staleJobs"`
	LeftoverPods  int                `json:"leftoverPods"`
	Findings      []Finding          `json:"findings"`
	ByNamespace   []NamespaceSummary `json:"byNamespace"`
}

// Options controls classification.
type Options struct {
	// MaxTTL is the largest TTL considered "sane". A finished Job with a TTL
	// greater than this is flagged as ttl-too-big. Zero means the threshold is
	// disabled: only a completely missing TTL is stale.
	MaxTTL time.Duration
	// Now is the reference time for completion-age math; the zero value means
	// time.Now() is used. Injectable for deterministic tests.
	Now time.Time
}

// Classify runs the pure audit over the given Jobs and Pods and returns a
// deterministic, sorted Report. It never mutates its inputs.
func Classify(jobs []Job, pods []Pod, opts Options) Report {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	// Count leftover Pods per owning Job UID.
	podsByJob := make(map[string]int, len(jobs))
	for _, p := range pods {
		if p.OwnerJobUID != "" {
			podsByJob[p.OwnerJobUID]++
		}
	}

	rep := Report{
		TotalJobs: len(jobs),
		Findings:  []Finding{},
	}
	if opts.MaxTTL > 0 {
		secs := int64(opts.MaxTTL / time.Second)
		rep.MaxTTLSeconds = &secs
	}

	nsAgg := map[string]*NamespaceSummary{}

	for _, j := range jobs {
		if !j.Finished {
			// Running or not-yet-finished Jobs are never flagged.
			continue
		}
		rep.FinishedJobs++

		reason, stale := classifyOne(j, opts.MaxTTL)
		if !stale {
			continue
		}

		pods := podsByJob[j.UID]
		f := Finding{
			Name:         j.Name,
			Namespace:    j.Namespace,
			Status:       j.FinishedReason,
			Reason:       reason,
			TTLSeconds:   j.TTLSeconds,
			LeftoverPods: pods,
		}
		if j.CompletionTime != nil {
			d := now.Sub(*j.CompletionTime)
			if d < 0 {
				d = 0
			}
			f.CompletionSecs = int64(d / time.Second)
			f.CompletionAge = humanDuration(d)
		} else {
			f.CompletionAge = "unknown"
		}
		if j.OwnerKind != "" && j.OwnerName != "" {
			f.Owner = j.OwnerKind + "/" + j.OwnerName
		}

		rep.Findings = append(rep.Findings, f)
		rep.StaleJobs++
		rep.LeftoverPods += pods

		s := nsAgg[j.Namespace]
		if s == nil {
			s = &NamespaceSummary{Namespace: j.Namespace}
			nsAgg[j.Namespace] = s
		}
		s.StaleJobs++
		s.LeftoverPods += pods
	}

	// Stable output: findings by (namespace, name); namespaces alphabetical.
	sort.Slice(rep.Findings, func(a, b int) bool {
		if rep.Findings[a].Namespace != rep.Findings[b].Namespace {
			return rep.Findings[a].Namespace < rep.Findings[b].Namespace
		}
		return rep.Findings[a].Name < rep.Findings[b].Name
	})

	rep.ByNamespace = make([]NamespaceSummary, 0, len(nsAgg))
	for _, s := range nsAgg {
		rep.ByNamespace = append(rep.ByNamespace, *s)
	}
	sort.Slice(rep.ByNamespace, func(a, b int) bool {
		return rep.ByNamespace[a].Namespace < rep.ByNamespace[b].Namespace
	})

	return rep
}

// classifyOne decides whether a single finished Job is stale.
func classifyOne(j Job, maxTTL time.Duration) (Reason, bool) {
	if j.TTLSeconds == nil {
		return ReasonNoTTL, true
	}
	if maxTTL > 0 {
		ttl := time.Duration(*j.TTLSeconds) * time.Second
		if ttl > maxTTL {
			return ReasonTTLTooBig, true
		}
	}
	return "", false
}

// humanDuration renders a duration compactly: "3d4h", "2h15m", "45s".
func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	days := int64(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int64(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	mins := int64(d / time.Minute)
	d -= time.Duration(mins) * time.Minute
	secs := int64(d / time.Second)

	switch {
	case days > 0:
		if hours > 0 {
			return itoa(days) + "d" + itoa(hours) + "h"
		}
		return itoa(days) + "d"
	case hours > 0:
		if mins > 0 {
			return itoa(hours) + "h" + itoa(mins) + "m"
		}
		return itoa(hours) + "h"
	case mins > 0:
		if secs > 0 {
			return itoa(mins) + "m" + itoa(secs) + "s"
		}
		return itoa(mins) + "m"
	default:
		return itoa(secs) + "s"
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
