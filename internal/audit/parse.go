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
	Spec     rawJobSpec `json:"spec"`
	Status   rawJobStat `json:"status"`
}

type rawMeta struct {
	Name            string        `json:"name"`
	Namespace       string        `json:"namespace"`
	UID             string        `json:"uid"`
	OwnerReferences []rawOwnerRef `json:"ownerReferences"`
}

type rawOwnerRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid"`
}

type rawJobSpec struct {
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished"`
}

type rawJobStat struct {
	Conditions     []rawCondition `json:"conditions"`
	CompletionTime *string        `json:"completionTime"`
}

type rawCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// ParseInput decodes a JSON or YAML dump into audit Jobs and Pods.
//
// The dump may be:
//   - a single List (kind: List) mixing Jobs and Pods,
//   - a JobList or PodList,
//   - or a bare {"items": [...]} array.
//
// Anything that is not a Job or a Pod is ignored, so a raw `kubectl get
// jobs,pods -A -o json` dump works directly.
func ParseInput(data []byte) ([]Job, []Pod, error) {
	var list rawList
	if err := yaml.Unmarshal(data, &list); err != nil {
		return nil, nil, fmt.Errorf("parse input: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, nil, fmt.Errorf("input has no items (expected a List with Jobs and Pods)")
	}

	var jobs []Job
	var pods []Pod
	for _, o := range list.Items {
		switch o.Kind {
		case "Job":
			jobs = append(jobs, toJob(o))
		case "Pod":
			pods = append(pods, toPod(o))
		default:
			// Ignore unrelated kinds.
		}
	}
	return jobs, pods, nil
}

func toJob(o rawObject) Job {
	j := Job{
		Name:       o.Metadata.Name,
		Namespace:  o.Metadata.Namespace,
		UID:        o.Metadata.UID,
		TTLSeconds: o.Spec.TTLSecondsAfterFinished,
	}
	for _, ref := range o.Metadata.OwnerReferences {
		if ref.Kind == "CronJob" {
			j.OwnerKind = ref.Kind
			j.OwnerName = ref.Name
			break
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
	}
	return j
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
