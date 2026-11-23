/*
Copyright 2026 The tierd Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
)

// modifyPVC is testKey's PVC, on currentTier and asked to move to specTier,
// with the given modify status and conditions.
func modifyPVC(specTier string, mvs *corev1.ModifyVolumeStatus,
	conds ...corev1.PersistentVolumeClaimCondition) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		Namespace: testKey.Namespace, Name: testKey.Name,
		Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: new(specTier)},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:              corev1.ClaimBound,
			Capacity:           corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			ModifyVolumeStatus: mvs,
			Conditions:         conds,
		},
	}
}

// modifyError is the condition drivers that never report Infeasible (Azure)
// set when a modify fails.
func modifyError(msg string) corev1.PersistentVolumeClaimCondition {
	return corev1.PersistentVolumeClaimCondition{
		Type: corev1.PersistentVolumeClaimVolumeModifyVolumeError, Status: corev1.ConditionTrue, Message: msg,
	}
}

func modifying(status corev1.PersistentVolumeClaimModifyVolumeStatus, target string) *corev1.ModifyVolumeStatus {
	return &corev1.ModifyVolumeStatus{Status: status, TargetVolumeAttributesClassName: target}
}

func TestCheckModify(t *testing.T) {
	const (
		driverMsg      = "disk does not support this tier"
		failedReason   = "Failed"
		driverRejected = "driver rejected the change" // checkModify's message when the driver gives none
	)
	tests := []struct {
		name     string
		mode     tierdv1.VolumeTierPolicyMode
		override bool
		pvc      *corev1.PersistentVolumeClaim

		wantStatus     metav1.ConditionStatus
		wantReason     string
		wantMessage    string
		wantInfeasible string // tier recorded as infeasible, with wantDriverMsg
		wantDriverMsg  string
		wantRollback   bool
	}{
		{
			name: "idle", mode: tierdv1.VolumeTierPolicyModeAct,
			pvc:        modifyPVC(tierBase, nil),
			wantStatus: metav1.ConditionFalse, wantReason: "Idle",
		},
		{
			name: "in progress", mode: tierdv1.VolumeTierPolicyModeAct,
			pvc:        modifyPVC(tierFast, modifying(corev1.PersistentVolumeClaimModifyVolumeInProgress, tierFast)),
			wantStatus: metav1.ConditionTrue, wantReason: "InProgress", wantMessage: "Moving to " + tierFast,
		},
		{
			name: "pending without a target names the PVC's class", mode: tierdv1.VolumeTierPolicyModeAct,
			pvc:        modifyPVC(tierFast, modifying(corev1.PersistentVolumeClaimModifyVolumePending, "")),
			wantStatus: metav1.ConditionTrue, wantReason: "Pending", wantMessage: "Moving to " + tierFast,
		},
		{
			name: "infeasible rolls back in act mode", mode: tierdv1.VolumeTierPolicyModeAct,
			pvc:        modifyPVC(tierFast, modifying(corev1.PersistentVolumeClaimModifyVolumeInfeasible, tierFast)),
			wantStatus: metav1.ConditionFalse, wantReason: failedReason, wantMessage: "Failed to move to " + tierFast,
			wantInfeasible: tierFast, wantDriverMsg: driverRejected, wantRollback: true,
		},
		{
			name: "infeasible only records in recommend mode", mode: tierdv1.VolumeTierPolicyModeRecommend,
			pvc:        modifyPVC(tierFast, modifying(corev1.PersistentVolumeClaimModifyVolumeInfeasible, tierFast)),
			wantStatus: metav1.ConditionFalse, wantReason: failedReason, wantMessage: "Failed to move to " + tierFast,
			wantInfeasible: tierFast, wantDriverMsg: driverRejected,
		},
		{
			name: "infeasible under manual override is not rolled back", mode: tierdv1.VolumeTierPolicyModeAct,
			override:   true,
			pvc:        modifyPVC(tierFast, modifying(corev1.PersistentVolumeClaimModifyVolumeInfeasible, tierFast)),
			wantStatus: metav1.ConditionFalse, wantReason: failedReason, wantMessage: "Failed to move to " + tierFast,
			wantInfeasible: tierFast, wantDriverMsg: driverRejected,
		},
		{
			name: "modify error while in progress is a failure", mode: tierdv1.VolumeTierPolicyModeAct,
			pvc: modifyPVC(tierFast, modifying(corev1.PersistentVolumeClaimModifyVolumeInProgress, tierFast),
				modifyError(driverMsg)),
			wantStatus: metav1.ConditionFalse, wantReason: failedReason, wantMessage: "Failed to move to " + tierFast,
			wantInfeasible: tierFast, wantDriverMsg: driverMsg, wantRollback: true,
		},
		{
			// The error is about a move back to the tier the volume is on.
			name: "modify error targeting the current tier is still in progress", mode: tierdv1.VolumeTierPolicyModeAct,
			pvc: modifyPVC(tierBase, modifying(corev1.PersistentVolumeClaimModifyVolumeInProgress, tierBase),
				modifyError(driverMsg)),
			wantStatus: metav1.ConditionTrue, wantReason: "InProgress", wantMessage: "Moving to " + tierBase,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vtp := &tierdv1.VolumeTierPolicy{
				Namespace: testKey.Namespace, Name: testPolicy,
				Spec: tierdv1.VolumeTierPolicySpec{Mode: tt.mode},
			}
			vts := &tierdv1.VolumeTierState{
				Namespace: testKey.Namespace, Name: testKey.Name,
				Status: tierdv1.VolumeTierStateStatus{PolicyRef: testPolicy, CurrentTier: tierBase},
			}
			if tt.override {
				setCondition(vts, ConditionVTSManualOverride, metav1.ConditionTrue, "Annotated", "")
			}
			vac := &storagev1.VolumeAttributesClass{Name: tierFast, UID: "vac-fast", DriverName: "d"}
			c, rec := modifyClient(t, interceptor.Funcs{}, tt.pvc, vac)
			r := &VolumeTierStateReconciler{Client: c, Recorder: rec}

			if err := r.checkModify(t.Context(), vtp, vts, tt.pvc); err != nil {
				t.Fatal(err)
			}

			cond := meta.FindStatusCondition(vts.Status.Conditions, ConditionVTSModifying)
			if cond == nil || cond.Status != tt.wantStatus || cond.Reason != tt.wantReason || cond.Message != tt.wantMessage {
				t.Errorf("Modifying = %+v, want %s/%s %q", cond, tt.wantStatus, tt.wantReason, tt.wantMessage)
			}

			inf := vts.Status.InfeasibleTiers
			switch {
			case tt.wantInfeasible == "" && len(inf) > 0:
				t.Errorf("infeasible tiers = %+v, want none", inf)
			case tt.wantInfeasible != "" && (len(inf) != 1 || inf[0].Name != tt.wantInfeasible ||
				inf[0].Message != tt.wantDriverMsg || inf[0].VACUID != vac.UID):
				t.Errorf("infeasible tiers = %+v, want %s (%s) with the class's UID", inf, tt.wantInfeasible, tt.wantDriverMsg)
			}

			stored := &corev1.PersistentVolumeClaim{}
			if err := c.Get(t.Context(), testKey, stored); err != nil {
				t.Fatal(err)
			}
			class := ptr.Deref(stored.Spec.VolumeAttributesClassName, "")
			if tt.wantRollback {
				if class != tierBase || stored.Annotations[AppliedTierAnnotation] != tierBase ||
					vts.Status.LastAppliedTier != tierBase {
					t.Errorf("after rollback: PVC class %q, applied annotation %q, LastAppliedTier %q; want %s",
						class, stored.Annotations[AppliedTierAnnotation], vts.Status.LastAppliedTier, tierBase)
				}
				if ev := <-rec.Events; !strings.Contains(ev, "TierChangeFailed") {
					t.Errorf("event = %q, want TierChangeFailed", ev)
				}
			} else if class != ptr.Deref(tt.pvc.Spec.VolumeAttributesClassName, "") {
				t.Errorf("PVC class changed to %q", class)
			}
		})
	}
}

func TestCheckModifyErrors(t *testing.T) {
	injected := errors.New("injected")
	pvc := func() *corev1.PersistentVolumeClaim {
		return modifyPVC(tierFast, modifying(corev1.PersistentVolumeClaimModifyVolumeInfeasible, tierFast))
	}
	vtp := &tierdv1.VolumeTierPolicy{Spec: tierdv1.VolumeTierPolicySpec{Mode: tierdv1.VolumeTierPolicyModeAct}}
	tests := []struct {
		name  string
		funcs interceptor.Funcs
	}{
		{"reading the class", interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch,
			key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*storagev1.VolumeAttributesClass); ok {
				return injected
			}
			return c.Get(ctx, key, obj, opts...)
		}}},
		{"rolling back", interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object,
			client.Patch, ...client.PatchOption) error {
			return injected
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := pvc()
			c, rec := modifyClient(t, tt.funcs, p)
			r := &VolumeTierStateReconciler{Client: c, Recorder: rec}
			vts := &tierdv1.VolumeTierState{Status: tierdv1.VolumeTierStateStatus{CurrentTier: tierBase}}
			if err := r.checkModify(t.Context(), vtp, vts, p); !errors.Is(err, injected) {
				t.Errorf("checkModify = %v, want the injected error", err)
			}
		})
	}
}

// A class deleted since the move started doesn't stop the failure being
// recorded.
func TestCheckModifyWithoutClass(t *testing.T) {
	p := modifyPVC(tierFast, modifying(corev1.PersistentVolumeClaimModifyVolumeInfeasible, tierFast))
	c, rec := modifyClient(t, interceptor.Funcs{}, p)
	r := &VolumeTierStateReconciler{Client: c, Recorder: rec}
	vtp := &tierdv1.VolumeTierPolicy{Spec: tierdv1.VolumeTierPolicySpec{Mode: tierdv1.VolumeTierPolicyModeRecommend}}
	vts := &tierdv1.VolumeTierState{Status: tierdv1.VolumeTierStateStatus{CurrentTier: tierBase}}

	if err := r.checkModify(t.Context(), vtp, vts, p); err != nil {
		t.Fatal(err)
	}
	if inf := vts.Status.InfeasibleTiers; len(inf) != 1 || inf[0].Name != tierFast || inf[0].VACUID != "" {
		t.Errorf("infeasible tiers = %+v, want fast without a class UID", inf)
	}
}

// modifyClient is a fake API server holding objs, and an event recorder.
func modifyClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) (client.Client, *events.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := tierdv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&tierdv1.VolumeTierState{}).Build()
	return interceptor.NewClient(c, funcs), events.NewFakeRecorder(10)
}

func TestEvaluate(t *testing.T) {
	vtp := &tierdv1.VolumeTierPolicy{Spec: tierdv1.VolumeTierPolicySpec{
		Tiers: []tierdv1.Tier{
			{VACName: tierBase, Ceilings: &tierdv1.Ceiling{IOPS: new(int64(1000))}},
			{VACName: tierFast, Ceilings: &tierdv1.Ceiling{IOPS: new(int64(5000))}},
		},
		ScaleUp: tierdv1.ScaleUp{UtilizationAbove: new(int32(80))},
	}}
	busy := &tierdv1.VolumeIOSample{Status: tierdv1.VolumeIOSampleStatus{IOPS: 900}}
	r := &VolumeTierStateReconciler{}

	if dir, why := r.Evaluate(vtp, tierBase, busy); dir != tierdv1.ScaleDirectionUp {
		t.Errorf("Evaluate on base at 90%% = %s (%s), want up", dir, why)
	}
	if dir, why := r.Evaluate(vtp, "other", busy); dir != tierdv1.ScaleDirectionNone || why != "current tier is not in the policy" {
		t.Errorf("Evaluate on a tier outside the policy = %s (%s), want none", dir, why)
	}
}
