package cluster

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/moveeeax/k8s-job-ttl-audit/internal/reap"
)

func ptr32(v int32) *int32 { return &v }

func objects() []runtime.Object {
	yes := true
	return []runtime.Object{
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "batch"},
			Spec:       batchv1.CronJobSpec{SuccessfulJobsHistoryLimit: ptr32(3), FailedJobsHistoryLimit: ptr32(1)},
		},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: "nightly-1", Namespace: "batch", UID: "job-1",
				Labels:          map[string]string{"team": "data"},
				OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: "nightly", Controller: &yes}},
			},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
		},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wf-step", Namespace: "batch", UID: "job-2",
				OwnerReferences: []metav1.OwnerReference{{Kind: "Workflow", Name: "wf", Controller: &yes}},
			},
		},
	}
}

func TestFetchListsCronJobs(t *testing.T) {
	c := &Client{cs: fake.NewClientset(objects()...)}
	inv, err := c.Fetch(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !inv.CronJobsListed || len(inv.CronJobs) != 1 {
		t.Fatalf("cronjobs = %+v (listed %v)", inv.CronJobs, inv.CronJobsListed)
	}
	if l := inv.CronJobs[0].SuccessfulJobsHistoryLimit; l == nil || *l != 3 {
		t.Errorf("successful limit = %v", l)
	}
	byName := map[string]int{}
	for i, j := range inv.Jobs {
		byName[j.Name] = i
	}
	nj := inv.Jobs[byName["nightly-1"]]
	if nj.OwnerName != "nightly" || nj.OtherOwner != "" || nj.Labels["team"] != "data" {
		t.Errorf("nightly-1 = %+v", nj)
	}
	if got := inv.Jobs[byName["wf-step"]].OtherOwner; got != "Workflow/wf" {
		t.Errorf("wf-step OtherOwner = %q", got)
	}
}

func TestFetchCronJobsForbiddenFallsBack(t *testing.T) {
	cs := fake.NewClientset(objects()...)
	cs.PrependReactor("list", "cronjobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "cronjobs"}, "", errors.New("rbac"))
	})
	var warn bytes.Buffer
	c := &Client{cs: cs, warn: &warn}
	inv, err := c.Fetch(context.Background(), "batch")
	if err != nil {
		t.Fatalf("forbidden cronjob list must not fail the scan: %v", err)
	}
	if inv.CronJobsListed || len(inv.Jobs) != 2 {
		t.Fatalf("inv = %+v", inv)
	}
	if !strings.Contains(warn.String(), "CronJob history limits ignored") {
		t.Errorf("warning = %q", warn.String())
	}
}

func TestDeleteJobBackgroundWithUIDPrecondition(t *testing.T) {
	cs := fake.NewClientset(objects()...)
	var got metav1.DeleteOptions
	cs.PrependReactor("delete", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		got = a.(k8stesting.DeleteAction).GetDeleteOptions()
		return false, nil, nil // let the tracker delete it
	})
	c := &Client{cs: cs}
	if err := c.DeleteJob(context.Background(), "batch", "nightly-1", "job-1"); err != nil {
		t.Fatal(err)
	}
	if got.PropagationPolicy == nil || *got.PropagationPolicy != metav1.DeletePropagationBackground {
		t.Errorf("propagation = %v, want Background", got.PropagationPolicy)
	}
	if got.Preconditions == nil || got.Preconditions.UID == nil || *got.Preconditions.UID != "job-1" {
		t.Errorf("preconditions = %+v, want UID job-1", got.Preconditions)
	}
	if _, err := cs.BatchV1().Jobs("batch").Get(context.Background(), "nightly-1", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("job still present: %v", err)
	}

	err := c.DeleteJob(context.Background(), "batch", "nightly-1", "job-1")
	if !errors.Is(err, reap.ErrGone) {
		t.Errorf("second delete = %v, want ErrGone", err)
	}
}
