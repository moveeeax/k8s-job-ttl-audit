package audit

import (
	"fmt"
	"time"

	"sigs.k8s.io/yaml"
)

// rawList is a Kubernetes List wrapper (kind: List) or a bare items array.
type rawList struct {
	Items []rawObject `json:"items"`
}

type rawObject struct {
	Kind     string     `json:"kind"`
	Metadata rawMeta    `json:"metadata"`
	Spec     rawSpec    `json:"spec"`
	Status   rawJobStat `json:"status"`
}

type rawMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	UID             string            `json:"uid"`
	Labels          map[string]string `json:"labels"`
	OwnerReferences []rawOwnerRef     `json:"ownerReferences"`
}

type rawOwnerRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid"`
	// Controller marks the managing owner; nil is read as false, as in the API.
	Controller *bool `json:"controller"`
}

// rawSpec holds the spec fields we read from Jobs and CronJobs; the two kinds
// share no field names, so one struct serves both.
type rawSpec struct {
	TTLSecondsAfterFinished    *int32 `json:"ttlSecondsAfterFinished"`
	SuccessfulJobsHistoryLimit *int32 `json:"successfulJobsHistoryLimit"`
	FailedJobsHistoryLimit     *int32 `json:"failedJobsHistoryLimit"`
}

type rawJobStat struct {
	Conditions     []rawCondition `json:"conditions"`
	CompletionTime *string        `json:"completionTime"`
}

type rawCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

// Inventory is everything one scan reads: Jobs, their Pods and the CronJobs
// that may own them.
type Inventory struct {
	Jobs     []Job
	Pods     []Pod
	CronJobs []CronJob
	// CronJobsListed is true when CronJobs were actually read for the scanned
	// scope, so a missing owner means the CronJob is gone.
	CronJobsListed bool
}

// ParseInput decodes a JSON or YAML dump into an Inventory.
//
// The dump may be:
//   - a single List (kind: List) mixing Jobs, Pods and CronJobs,
//   - a JobList or PodList,
//   - or a bare {"items": [...]} array.
//
// Anything that is not a Job, Pod or CronJob is ignored, so a raw `kubectl get
// jobs,pods,cronjobs -A -o json` dump works directly. A dump without any
// CronJob leaves CronJobsListed false: history limits are then not applied.
func ParseInput(data []byte) (Inventory, error) {
	var list rawList
	if err := yaml.Unmarshal(data, &list); err != nil {
		return Inventory{}, fmt.Errorf("parse input: %w", err)
	}
	if len(list.Items) == 0 {
		return Inventory{}, fmt.Errorf("input has no items (expected a List with Jobs and Pods)")
	}

	var inv Inventory
	for _, o := range list.Items {
		switch o.Kind {
		case "Job":
			inv.Jobs = append(inv.Jobs, toJob(o))
		case "Pod":
			inv.Pods = append(inv.Pods, toPod(o))
		case "CronJob":
			inv.CronJobs = append(inv.CronJobs, toCronJob(o))
		default:
			// Ignore unrelated kinds.
		}
	}
	inv.CronJobsListed = len(inv.CronJobs) > 0
	return inv, nil
}

func toJob(o rawObject) Job {
	j := Job{
		Name:       o.Metadata.Name,
		Namespace:  o.Metadata.Namespace,
		UID:        o.Metadata.UID,
		TTLSeconds: o.Spec.TTLSecondsAfterFinished,
		Labels:     o.Metadata.Labels,
	}
	for _, ref := range o.Metadata.OwnerReferences {
		if ref.Kind == "CronJob" {
			j.OwnerKind = ref.Kind
			j.OwnerName = ref.Name
			j.OwnerUID = ref.UID
			j.OtherOwner = ""
			break
		}
		if ref.Controller != nil && *ref.Controller {
			j.OtherOwner = ref.Kind + "/" + ref.Name
		}
	}
	for _, c := range o.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		if c.Type == "Complete" || c.Type == "Failed" {
			j.Finished = true
			j.FinishedReason = c.Type
			break
		}
	}
	if o.Status.CompletionTime != nil {
		if t, err := time.Parse(time.RFC3339, *o.Status.CompletionTime); err == nil {
			j.CompletionTime = &t
		}
	} else if j.FinishedReason == "Failed" {
		// Failed Jobs may lack completionTime; fall back to the condition's
		// last transition, as the live adapter does.
		for _, c := range o.Status.Conditions {
			if c.Type == "Failed" && c.Status == "True" {
				if t, err := time.Parse(time.RFC3339, c.LastTransitionTime); err == nil {
					j.CompletionTime = &t
				}
				break
			}
		}
	}
	return j
}

func toCronJob(o rawObject) CronJob {
	return CronJob{
		Name:                       o.Metadata.Name,
		Namespace:                  o.Metadata.Namespace,
		UID:                        o.Metadata.UID,
		SuccessfulJobsHistoryLimit: o.Spec.SuccessfulJobsHistoryLimit,
		FailedJobsHistoryLimit:     o.Spec.FailedJobsHistoryLimit,
	}
}

func toPod(o rawObject) Pod {
	p := Pod{
		Name:      o.Metadata.Name,
		Namespace: o.Metadata.Namespace,
	}
	for _, ref := range o.Metadata.OwnerReferences {
		if ref.Kind == "Job" {
			p.OwnerJobUID = ref.UID
			break
		}
	}
	return p
}
