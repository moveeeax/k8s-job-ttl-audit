// Package cluster reads live Jobs, Pods and CronJobs from a Kubernetes cluster
// via client-go, adapts them into the audit package's plain types, and deletes
// Jobs for the reap command.
package cluster

import (
	"context"
	"fmt"
	"io"
	"os"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/moveeeax/k8s-job-ttl-audit/internal/audit"
	"github.com/moveeeax/k8s-job-ttl-audit/internal/reap"
)

// Client wraps a Kubernetes clientset.
type Client struct {
	cs   kubernetes.Interface
	warn io.Writer // non-fatal problems, e.g. no RBAC to list CronJobs
}

// New builds a Client from the given kubeconfig path (empty = default loading
// rules: $KUBECONFIG then ~/.kube/config) and context override (empty = current).
func New(kubeconfig, contextName string) (*Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	return &Client{cs: cs, warn: os.Stderr}, nil
}

// Fetch lists Jobs, Pods and CronJobs. namespace "" means all namespaces.
//
// When listing CronJobs is forbidden, Fetch warns and returns the Inventory
// with CronJobsListed false, so the audit still runs without history limits
// instead of reporting every CronJob Job as orphaned.
func (c *Client) Fetch(ctx context.Context, namespace string) (audit.Inventory, error) {
	jobList, err := c.cs.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return audit.Inventory{}, fmt.Errorf("list jobs: %w", err)
	}
	podList, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return audit.Inventory{}, fmt.Errorf("list pods: %w", err)
	}

	inv := audit.Inventory{
		Jobs: make([]audit.Job, 0, len(jobList.Items)),
		Pods: make([]audit.Pod, 0, len(podList.Items)),
	}
	for i := range jobList.Items {
		inv.Jobs = append(inv.Jobs, adaptJob(&jobList.Items[i]))
	}
	for i := range podList.Items {
		inv.Pods = append(inv.Pods, adaptPod(&podList.Items[i]))
	}

	cronList, err := c.cs.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	switch {
	case apierrors.IsForbidden(err):
		if c.warn != nil {
			fmt.Fprintf(c.warn, "warning: cannot list cronjobs, CronJob history limits ignored: %v\n", err)
		}
	case err != nil:
		return audit.Inventory{}, fmt.Errorf("list cronjobs: %w", err)
	default:
		inv.CronJobsListed = true
		inv.CronJobs = make([]audit.CronJob, 0, len(cronList.Items))
		for i := range cronList.Items {
			inv.CronJobs = append(inv.CronJobs, adaptCronJob(&cronList.Items[i]))
		}
	}
	return inv, nil
}

// DeleteJob deletes one Job with background propagation, so the garbage
// collector removes its Pods instead of leaving them orphaned. The UID
// precondition makes the call fail rather than delete a newer Job that reused
// the name since it was listed.
func (c *Client) DeleteJob(ctx context.Context, namespace, name, uid string) error {
	policy := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &policy}
	if uid != "" {
		u := types.UID(uid)
		opts.Preconditions = &metav1.Preconditions{UID: &u}
	}
	if err := c.cs.BatchV1().Jobs(namespace).Delete(ctx, name, opts); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("delete job %s/%s: %w", namespace, name, reap.ErrGone)
		}
		return fmt.Errorf("delete job %s/%s: %w", namespace, name, err)
	}
	return nil
}

func adaptCronJob(cj *batchv1.CronJob) audit.CronJob {
	return audit.CronJob{
		Name:                       cj.Name,
		Namespace:                  cj.Namespace,
		UID:                        string(cj.UID),
		SuccessfulJobsHistoryLimit: cj.Spec.SuccessfulJobsHistoryLimit,
		FailedJobsHistoryLimit:     cj.Spec.FailedJobsHistoryLimit,
	}
}

func adaptJob(j *batchv1.Job) audit.Job {
	out := audit.Job{
		Name:       j.Name,
		Namespace:  j.Namespace,
		UID:        string(j.UID),
		TTLSeconds: j.Spec.TTLSecondsAfterFinished,
		Labels:     j.Labels,
	}
	for _, ref := range j.OwnerReferences {
		if ref.Kind == "CronJob" {
			out.OwnerKind = ref.Kind
			out.OwnerName = ref.Name
			out.OwnerUID = string(ref.UID)
			out.OtherOwner = ""
			break
		}
		if ref.Controller != nil && *ref.Controller {
			out.OtherOwner = ref.Kind + "/" + ref.Name
		}
	}
	for _, cond := range j.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		if cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed {
			out.Finished = true
			out.FinishedReason = string(cond.Type)
			break
		}
	}
	if j.Status.CompletionTime != nil {
		t := j.Status.CompletionTime.Time
		out.CompletionTime = &t
	} else if out.Finished {
		// Failed Jobs may lack completionTime; fall back to the condition's
		// last transition so age math still works.
		for _, cond := range j.Status.Conditions {
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				t := cond.LastTransitionTime.Time
				if !t.IsZero() {
					out.CompletionTime = &t
				}
				break
			}
		}
	}
	return out
}

func adaptPod(p *corev1.Pod) audit.Pod {
	out := audit.Pod{
		Name:      p.Name,
		Namespace: p.Namespace,
	}
	for _, ref := range p.OwnerReferences {
		if ref.Kind == "Job" {
			out.OwnerJobUID = string(ref.UID)
			break
		}
	}
	return out
}
