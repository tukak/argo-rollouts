package controller

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/utils/pointer"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/argoproj/argo-rollouts/pkg/client/clientset/versioned/fake"
	"github.com/argoproj/argo-rollouts/utils/annotations"
	"github.com/argoproj/argo-rollouts/utils/conditions"
)

var (
	noTimestamp = metav1.Time{}
)

func newBlueGreenRollout(name string, replicas int, revisionHistoryLimit *int32, stepIndex *int32, activeSvc string, previewSvc string) *v1alpha1.Rollout {
	rollout := newRollout(name, replicas, revisionHistoryLimit, map[string]string{"foo": "bar"})
	rollout.Spec.Strategy.BlueGreenStrategy = &v1alpha1.BlueGreenStrategy{
		ActiveService:  activeSvc,
		PreviewService: previewSvc,
	}
	rollout.Status.CurrentStepIndex = stepIndex
	rollout.Status.CurrentStepHash = conditions.ComputeStepHash(rollout)
	rollout.Status.CurrentPodHash = controller.ComputeHash(&rollout.Spec.Template, rollout.Status.CollisionCount)
	return rollout
}

func newAvailableCondition(available bool) ([]v1alpha1.RolloutCondition, string) {
	message := "Rollout is not serving traffic from the active service."
	status := corev1.ConditionFalse
	if available {
		message = "Rollout is serving traffic from the active service."
		status = corev1.ConditionTrue

	}
	rc := []v1alpha1.RolloutCondition{{
		LastTransitionTime: metav1.Now(),
		LastUpdateTime:     metav1.Now(),
		Message:            message,
		Reason:             "Available",
		Status:             status,
		Type:               v1alpha1.RolloutAvailable,
	}}
	rcStr, _ := json.Marshal(rc)
	return rc, string(rcStr)
}

func TestBlueGreenReconcileVerifyingPreview(t *testing.T) {
	boolPtr := func(boolean bool) *bool { return &boolean }
	tests := []struct {
		name                 string
		activeSvc            *corev1.Service
		previewSvcName       string
		verifyingPreviewFlag *bool
		notFinishedVerifying bool
	}{
		{
			name:                 "Continue if preview Service isn't specificed",
			activeSvc:            newService("active", 80, nil),
			verifyingPreviewFlag: boolPtr(true),
			notFinishedVerifying: false,
		},
		{
			name:                 "Continue if active service doesn't have a selector from the rollout",
			previewSvcName:       "previewSvc",
			activeSvc:            newService("active", 80, nil),
			verifyingPreviewFlag: boolPtr(true),
			notFinishedVerifying: false,
		},
		{
			name:                 "Do not continue if verifyingPreview flag is true",
			previewSvcName:       "previewSvc",
			activeSvc:            newService("active", 80, map[string]string{v1alpha1.DefaultRolloutUniqueLabelKey: "test"}),
			verifyingPreviewFlag: boolPtr(true),
			notFinishedVerifying: true,
		},
		{
			name:                 "Continue if verifyingPreview flag is false",
			previewSvcName:       "previewSvc",
			activeSvc:            newService("active", 80, map[string]string{v1alpha1.DefaultRolloutUniqueLabelKey: "test"}),
			verifyingPreviewFlag: boolPtr(false),
			notFinishedVerifying: false,
		},
		{
			name:                 "Continue if verifyingPreview flag is not set",
			previewSvcName:       "previewSvc",
			activeSvc:            newService("active", 80, map[string]string{v1alpha1.DefaultRolloutUniqueLabelKey: "test"}),
			notFinishedVerifying: false,
		},
	}
	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			rollout := newBlueGreenRollout("foo", 1, nil, nil, "", test.previewSvcName)
			rollout.Status = v1alpha1.RolloutStatus{
				VerifyingPreview: test.verifyingPreviewFlag,
			}
			fake := fake.Clientset{}
			k8sfake := k8sfake.Clientset{}
			controller := &Controller{
				rolloutsclientset: &fake,
				kubeclientset:     &k8sfake,
				recorder:          &record.FakeRecorder{},
			}
			finishedVerifying := controller.reconcileVerifyingPreview(test.activeSvc, rollout)
			assert.Equal(t, test.notFinishedVerifying, finishedVerifying)
		})
	}
}

func TestBlueGreenHandlePreviewWhenActiveSet(t *testing.T) {
	f := newFixture(t)

	r1 := newBlueGreenRollout("foo", 1, nil, map[string]string{"foo": "bar"}, "preview", "active")

	r2 := r1.DeepCopy()
	annotations.SetRolloutRevision(r2, "2")
	r2.Spec.Template.Spec.Containers[0].Image = "foo/bar2.0"
	f.rolloutLister = append(f.rolloutLister, r2)
	f.objects = append(f.objects, r2)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-6479c8f85c", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs2)

	previewSvc := newService("preview", 80, map[string]string{v1alpha1.DefaultRolloutUniqueLabelKey: "895c6c4f9"})
	f.kubeobjects = append(f.kubeobjects, previewSvc)

	activeSvc := newService("active", 80, map[string]string{v1alpha1.DefaultRolloutUniqueLabelKey: "6479c8f85c"})
	f.kubeobjects = append(f.kubeobjects, activeSvc)

	f.expectGetServiceAction(previewSvc)
	f.expectGetServiceAction(activeSvc)
	f.expectPatchServiceAction(previewSvc, "")
	f.expectPatchRolloutAction(r2)
	f.run(getKey(r2, t))
}

func TestBlueGreenHandleVerifyingPreviewSetButNotPreviewSvc(t *testing.T) {
	f := newFixture(t)

	r1 := newBlueGreenRollout("foo", 1, nil, map[string]string{"foo": "bar"}, "active", "preview")
	r2 := r1.DeepCopy()
	annotations.SetRolloutRevision(r2, "2")
	r2.Spec.Template.Spec.Containers[0].Image = "foo/bar2.0"
	f.rolloutLister = append(f.rolloutLister, r2)
	f.objects = append(f.objects, r2)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-6479c8f85c", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs2)

	previewSvc := newService("preview", 80, map[string]string{v1alpha1.DefaultRolloutUniqueLabelKey: ""})
	f.kubeobjects = append(f.kubeobjects, previewSvc)

	activeSvc := newService("active", 80, map[string]string{v1alpha1.DefaultRolloutUniqueLabelKey: "895c6c4f9"})
	f.kubeobjects = append(f.kubeobjects, activeSvc)

	r2.Status.VerifyingPreview = func(boolean bool) *bool { return &boolean }(true)

	f.expectGetServiceAction(previewSvc)
	f.expectGetServiceAction(activeSvc)
	f.expectPatchRolloutAction(r2)
	f.expectPatchServiceAction(previewSvc, "")
	f.expectPatchRolloutAction(r2)
	f.run(getKey(r2, t))
}

func TestBlueGreenCreatesReplicaSet(t *testing.T) {
	f := newFixture(t)

	r := newBlueGreenRollout("foo", 1, nil, nil, "bar", "")
	f.rolloutLister = append(f.rolloutLister, r)
	f.objects = append(f.objects, r)
	s := newService("bar", 80, nil)
	f.kubeobjects = append(f.kubeobjects, s)

	rs := newReplicaSet(r, "foo-895c6c4f9", 1)

	f.expectCreateReplicaSetAction(rs)
	f.expectGetServiceAction(s)
	f.expectPatchRolloutAction(r)
	f.run(getKey(r, t))
}

func TestBlueGreenSetPreviewService(t *testing.T) {
	f := newFixture(t)

	r := newBlueGreenRollout("foo", 1, nil, pointer.Int32Ptr(0), "active", "preview")
	f.rolloutLister = append(f.rolloutLister, r)
	f.objects = append(f.objects, r)

	rs := newReplicaSetWithStatus(r, "foo-895c6c4f9", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs)
	f.replicaSetLister = append(f.replicaSetLister, rs)

	previewSvc := newService("preview", 80, nil)
	selector := map[string]string{v1alpha1.DefaultRolloutUniqueLabelKey: "test"}
	activeSvc := newService("active", 80, selector)
	f.kubeobjects = append(f.kubeobjects, previewSvc, activeSvc)

	f.expectGetServiceAction(activeSvc)
	f.expectGetServiceAction(previewSvc)
	f.expectPatchServiceAction(previewSvc, "")
	f.expectPatchRolloutAction(r)
	f.expectPatchRolloutAction(r)
	f.run(getKey(r, t))
}

func TestBlueGreenVerifyPreviewNoActions(t *testing.T) {
	f := newFixture(t)

	r := newBlueGreenRollout("foo", 1, nil, pointer.Int32Ptr(1), "active", "preview")
	r.Status.VerifyingPreview = func(boolean bool) *bool { return &boolean }(true)
	f.rolloutLister = append(f.rolloutLister, r)
	f.objects = append(f.objects, r)

	rs := newReplicaSetWithStatus(r, "foo-895c6c4f9", 1, 1)
	rs2 := newImage(rs, "foo/bar2.0")
	f.kubeobjects = append(f.kubeobjects, rs, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs, rs2)

	previewSelector := map[string]string{v1alpha1.DefaultRolloutUniqueLabelKey: "895c6c4f9"}
	previewSvc := newService("preview", 80, previewSelector)
	activeSelector := map[string]string{v1alpha1.DefaultRolloutUniqueLabelKey: rs2.Name}
	activeSvc := newService("active", 80, activeSelector)
	f.kubeobjects = append(f.kubeobjects, previewSvc, activeSvc)

	f.expectGetServiceAction(activeSvc)
	f.expectGetServiceAction(previewSvc)
	f.expectPatchRolloutAction(r)
	f.run(getKey(r, t))
}

func TestBlueGreenSkipPreviewUpdateActive(t *testing.T) {
	f := newFixture(t)

	r := newBlueGreenRollout("foo", 1, nil, nil, "active", "preview")
	r.Status.AvailableReplicas = 1
	f.rolloutLister = append(f.rolloutLister, r)
	f.objects = append(f.objects, r)

	rs := newReplicaSetWithStatus(r, "foo-895c6c4f9", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs)
	f.replicaSetLister = append(f.replicaSetLister, rs)

	previewSvc := newService("preview", 80, nil)
	activeSvc := newService("active", 80, nil)
	f.kubeobjects = append(f.kubeobjects, previewSvc, activeSvc)

	f.expectGetServiceAction(activeSvc)
	f.expectGetServiceAction(previewSvc)
	f.expectPatchServiceAction(activeSvc, rs.Labels[v1alpha1.DefaultRolloutUniqueLabelKey])
	f.expectPatchRolloutAction(r)
	f.run(getKey(r, t))
}

func TestBlueGreenScaleDownOldRS(t *testing.T) {
	f := newFixture(t)

	r1 := newBlueGreenRollout("foo", 1, nil, pointer.Int32Ptr(3), "bar", "")

	r2 := bumpVersion(r1)
	f.rolloutLister = append(f.rolloutLister, r2)
	f.objects = append(f.objects, r2)

	rs1 := newReplicaSetWithStatus(r1, "foo-895c6c4f9", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs1)
	f.replicaSetLister = append(f.replicaSetLister, rs1)

	rs2 := newReplicaSetWithStatus(r2, "foo-5f79b78d7f", 1, 1)
	f.kubeobjects = append(f.kubeobjects, rs2)
	f.replicaSetLister = append(f.replicaSetLister, rs2)
	rs2PodHash := rs2.Labels[v1alpha1.DefaultRolloutUniqueLabelKey]

	serviceSelector := map[string]string{v1alpha1.DefaultRolloutUniqueLabelKey: rs2PodHash}
	s := newService("bar", 80, serviceSelector)
	f.kubeobjects = append(f.kubeobjects, s)

	expRS := rs2.DeepCopy()
	expRS.Annotations[annotations.DesiredReplicasAnnotation] = "0"
	f.expectGetServiceAction(s)
	f.expectUpdateReplicaSetAction(expRS)
	f.expectPatchRolloutAction(r1)

	f.run(getKey(r2, t))
}
