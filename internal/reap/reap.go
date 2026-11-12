// Package reap plans and executes deletion of finished Jobs older than a
// retention window, keeping the newest N Jobs of every owning CronJob.
//
// Planning is a pure function over audit.Job values, so it runs offline
// against fixtures exactly like the audit classifier. Only Execute talks to
// the cluster, through the Deleter interface.
package reap

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/moveeeax/k8s-job-ttl-audit/internal/audit"
)

// Options controls which finished Jobs are reaped.
type Options struct {
	// OlderThan is the retention window: a Job is reaped only when it finished
	// strictly longer ago than this.
	OlderThan time.Duration
	// KeepLast is how many of the newest finished Jobs of each owning CronJob
	// are never reaped, whatever their age. Jobs without a CronJob owner are
	// not grouped and only the window applies to them.
	KeepLast int
	// Selector limits reaping to Jobs whose labels match; nil matches all.
	Selector labels.Selector
	// Exclude lists Jobs that are never reaped: "name" or "namespace/name",
	// each part a path.Match glob ("nightly-*", "prod/*").
	Exclude []string
	// Now is the reference time for age math; zero means time.Now().
	Now time.Time
}

// Candidate is one Job the plan would delete.
type Candidate struct {
	Name           string    `json:"name"`
	Namespace      string    `json:"namespace"`
	UID            string    `json:"uid"`
	Status         string    `json:"status"` // Complete | Failed
	Owner          string    `json:"owner,omitempty"`
	CompletionTime time.Time `json:"completionTime"`
	Age            string    `json:"age"`
	AgeSeconds     int64     `json:"ageSeconds"`
}

// Plan is the result of planning one sweep.
type Plan struct {
	OlderThanSeconds int64       `json:"olderThanSeconds"`
	KeepLast         int         `json:"keepLast"`
	TotalJobs        int         `json:"totalJobs"`
	FinishedJobs     int         `json:"finishedJobs"`
	Candidates       []Candidate `json:"candidates"`

	// Why finished Jobs were left alone. A Job is counted under the first
	// rule that keeps it, in this order.
	KeptLast     int `json:"keptLast"`     // among the newest KeepLast of its CronJob
	OtherOwner   int `json:"otherOwner"`   // managed by a non-CronJob controller
	UnknownAge   int `json:"unknownAge"`   // no completion time to age it by
	WithinWindow int `json:"withinWindow"` // finished less than OlderThan ago
	NotSelected  int `json:"notSelected"`  // labels do not match Selector
	Excluded     int `json:"excluded"`     // matched an Exclude pattern
}

// ValidateExclude checks that every exclude pattern is a well-formed glob.
func ValidateExclude(patterns []string) error {
	for _, p := range patterns {
		ns, name, ok := strings.Cut(p, "/")
		if !ok {
			name, ns = ns, ""
		}
		if name == "" || strings.Contains(name, "/") {
			return fmt.Errorf("invalid --exclude %q: want name or namespace/name", p)
		}
		for _, part := range []string{ns, name} {
			if _, err := path.Match(part, ""); err != nil {
				return fmt.Errorf("invalid --exclude %q: %w", p, err)
			}
		}
	}
	return nil
}

// PlanReap decides which Jobs to delete. It never mutates its input and
// returns candidates sorted by (namespace, name).
func PlanReap(jobs []audit.Job, opts Options) Plan {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	plan := Plan{
		OlderThanSeconds: int64(opts.OlderThan / time.Second),
		KeepLast:         opts.KeepLast,
		TotalJobs:        len(jobs),
		Candidates:       []Candidate{},
	}

	finished := make([]audit.Job, 0, len(jobs))
	for _, j := range jobs {
		if j.Finished {
			finished = append(finished, j)
		}
	}
	plan.FinishedJobs = len(finished)
	kept := keepLastSet(finished, opts.KeepLast)

	for _, j := range finished {
		switch {
		case kept[j.UID+"\x00"+j.Namespace+"/"+j.Name]:
			plan.KeptLast++
			continue
		case j.OtherOwner != "":
			plan.OtherOwner++
			continue
		case j.CompletionTime == nil:
			plan.UnknownAge++
			continue
		}
		age := now.Sub(*j.CompletionTime)
		switch {
		case age <= opts.OlderThan:
			plan.WithinWindow++
			continue
		case opts.Selector != nil && !opts.Selector.Matches(labels.Set(j.Labels)):
			plan.NotSelected++
			continue
		case excluded(j, opts.Exclude):
			plan.Excluded++
			continue
		}
		c := Candidate{
			Name:           j.Name,
			Namespace:      j.Namespace,
			UID:            j.UID,
			Status:         j.FinishedReason,
			CompletionTime: *j.CompletionTime,
			Age:            audit.HumanDuration(age),
			AgeSeconds:     int64(age / time.Second),
		}
		if j.OwnerKind != "" && j.OwnerName != "" {
			c.Owner = j.OwnerKind + "/" + j.OwnerName
		}
		plan.Candidates = append(plan.Candidates, c)
	}

	sort.Slice(plan.Candidates, func(a, b int) bool {
		if plan.Candidates[a].Namespace != plan.Candidates[b].Namespace {
			return plan.Candidates[a].Namespace < plan.Candidates[b].Namespace
		}
		return plan.Candidates[a].Name < plan.Candidates[b].Name
	})
	return plan
}

// keepLastSet returns the keys of the newest keepLast finished Jobs of every
// owning CronJob. Jobs without a completion time sort as newest, so a Job that
// cannot be dated is kept rather than pushing a dated one out of the set.
func keepLastSet(finished []audit.Job, keepLast int) map[string]bool {
	kept := map[string]bool{}
	if keepLast <= 0 {
		return kept
	}
	groups := map[string][]audit.Job{}
	for _, j := range finished {
		if j.OwnerKind == "CronJob" && j.OwnerName != "" {
			k := j.Namespace + "/" + j.OwnerName
			groups[k] = append(groups[k], j)
		}
	}
	for _, g := range groups {
		sort.Slice(g, func(a, b int) bool {
			ta, tb := g[a].CompletionTime, g[b].CompletionTime
			switch {
			case ta == nil && tb == nil:
				return g[a].Name > g[b].Name
			case ta == nil:
				return true
			case tb == nil:
				return false
			case !ta.Equal(*tb):
				return ta.After(*tb)
			}
			return g[a].Name > g[b].Name
		})
		for i := 0; i < len(g) && i < keepLast; i++ {
			kept[g[i].UID+"\x00"+g[i].Namespace+"/"+g[i].Name] = true
		}
	}
	return kept
}

func excluded(j audit.Job, patterns []string) bool {
	for _, p := range patterns {
		nsPat, namePat, ok := strings.Cut(p, "/")
		if !ok {
			namePat, nsPat = nsPat, "*"
		}
		nsOK, _ := path.Match(nsPat, j.Namespace)
		nameOK, _ := path.Match(namePat, j.Name)
		if nsOK && nameOK {
			return true
		}
	}
	return false
}

// Deleter deletes one Job; cluster.Client implements it.
type Deleter interface {
	DeleteJob(ctx context.Context, namespace, name, uid string) error
}

// ErrGone is returned (wrapped) by a Deleter when the Job no longer exists,
// for example because the TTL controller removed it after it was listed.
var ErrGone = errors.New("job already gone")

// Result is the outcome of executing a plan.
type Result struct {
	Deleted int
	Gone    int // already deleted by someone else; not an error
	Errors  []error
}

// Execute deletes every candidate in the plan, continuing past failures so
// one stuck Job does not block the rest of the sweep.
func Execute(ctx context.Context, plan Plan, d Deleter, m *Metrics) Result {
	var res Result
	for _, c := range plan.Candidates {
		if err := ctx.Err(); err != nil {
			res.Errors = append(res.Errors, err)
			break
		}
		if err := d.DeleteJob(ctx, c.Namespace, c.Name, c.UID); err != nil {
			if errors.Is(err, ErrGone) {
				res.Gone++
				continue
			}
			res.Errors = append(res.Errors, err)
			m.AddError()
			continue
		}
		res.Deleted++
		m.AddReaped(c.Namespace)
	}
	return res
}
