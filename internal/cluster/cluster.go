// Package cluster reads live Jobs and Pods from a Kubernetes cluster via
// client-go and adapts them into the audit package's plain types.
package cluster

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/moveeeax/k8s-job-ttl-audit/internal/audit"
)

// Client wraps a Kubernetes clientset.
type Client struct {
	cs kubernetes.Interface
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
	return &Client{cs: cs}, nil
}

// Fetch lists Jobs and Pods. namespace "" means all namespaces.
func (c *Client) Fetch(ctx context.Context, namespace string) ([]audit.Job, []audit.Pod, error) {
	jobList, err := c.cs.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list jobs: %w", err)
	}
	podList, err := c.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("list pods: %w", err)
	}

	jobs := make([]audit.Job, 0, len(jobList.Items))
	for i := range jobList.Items {
		jobs = append(jobs, adaptJob(&jobList.Items[i]))
	}
	pods := make([]audit.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pods = append(pods, adaptPod(&podList.Items[i]))
	}
	return jobs, pods, nil
}

func adaptJob(j *batchv1.Job) audit.Job {
	out := audit.Job{
		Name:       j.Name,
		Namespace:  j.Namespace,
		UID:        string(j.UID),
		TTLSeconds: j.Spec.TTLSecondsAfterFinished,
	}
	for _, ref := range j.OwnerReferences {
		if ref.Kind == "CronJob" {
			out.OwnerKind = ref.Kind
			out.OwnerName = ref.Name
			break
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
