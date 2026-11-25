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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	tierdv1alpha1 "github.com/zanderyk/tierd/api/v1alpha1"
)

const testDriver = "disk.csi.example.com"

// vtsFixture is the cluster a VolumeTierState reconcile sees. Each field
// is nil when the object doesn't exist.
type vtsFixture struct {
	pvc    *corev1.PersistentVolumeClaim
	pv     *corev1.PersistentVolume
	vac    *storagev1.VolumeAttributesClass
	vtp    *tierdv1alpha1.VolumeTierPolicy
	vts    *tierdv1alpha1.VolumeTierState
	sample *tierdv1alpha1.VolumeIOSample
}

// newVTSFixture is a bound PVC on CSI volume "pv", not yet on a tier,
// managed by an Act policy over base (1000 IOPS) and fast (5000 IOPS), whose
// sample shows 900 IOPS: over 80% of base.
func newVTSFixture() *vtsFixture {
	return &vtsFixture{
		pvc: &corev1.PersistentVolumeClaim{
			Namespace: testKey.Namespace, Name: testKey.Name,
			Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: "pv"},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		},
		pv: &corev1.PersistentVolume{
			Name: "pv",
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: testDriver},
			}},
		},
		vac: &storagev1.VolumeAttributesClass{Name: tierBase, DriverName: testDriver},
		vtp: &tierdv1alpha1.VolumeTierPolicy{
			Namespace: testKey.Namespace, Name: testPolicy,
			Spec: tierdv1alpha1.VolumeTierPolicySpec{
				Mode: tierdv1alpha1.VolumeTierPolicyModeAct,
				Tiers: []tierdv1alpha1.Tier{
					{VACName: tierBase, Ceilings: &tierdv1alpha1.Ceiling{IOPS: new(int64(1000))}},
					{VACName: tierFast, Ceilings: &tierdv1alpha1.Ceiling{IOPS: new(int64(5000))}},
				},
				ScaleUp: tierdv1alpha1.ScaleUp{UtilizationAbove: new(int32(80))},
			},
		},
		vts: &tierdv1alpha1.VolumeTierState{
			Namespace: testKey.Namespace, Name: testKey.Name,
			Status: tierdv1alpha1.VolumeTierStateStatus{PolicyRef: testPolicy},
		},
		sample: &tierdv1alpha1.VolumeIOSample{
			Namespace: testKey.Namespace, Name: testKey.Name,
			Status: tierdv1alpha1.VolumeIOSampleStatus{IOPS: 900},
		},
	}
}

// onTier puts the PVC on tier.
func (f *vtsFixture) onTier(tier string) *vtsFixture {
	f.pvc.Spec.VolumeAttributesClassName = new(tier)
	f.pvc.Status.CurrentVolumeAttributesClassName = new(tier)
	return f
}

func (f *vtsFixture) objects() []client.Object {
	var objs []client.Object
	add := func(present bool, o client.Object) {
		if present {
			objs = append(objs, o)
		}
	}
	add(f.pvc != nil, f.pvc)
	add(f.pv != nil, f.pv)
	add(f.vac != nil, f.vac)
	add(f.vtp != nil, f.vtp)
	add(f.vts != nil, f.vts)
	add(f.sample != nil, f.sample)
	return objs
}

func TestReconcileStateManaged(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(f *vtsFixture)

		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage string // substring
	}{
		{
			name:       "PVC gone",
			mutate:     func(f *vtsFixture) { f.pvc = nil },
			wantStatus: metav1.ConditionFalse, wantReason: ConditionReasonInvalidPVC, wantMessage: "not found",
		},
		{
			name:       "PVC not bound",
			mutate:     func(f *vtsFixture) { f.pvc.Status.Phase = corev1.ClaimPending },
			wantStatus: metav1.ConditionFalse, wantReason: ConditionReasonInvalidPVC, wantMessage: errPVCNotBound.Error(),
		},
		{
			name:       "no policy assigned",
			mutate:     func(f *vtsFixture) { f.vts.Status.PolicyRef = "" },
			wantStatus: metav1.ConditionFalse, wantReason: ConditionReasonMissingPolicy, wantMessage: "policy ref is empty",
		},
		{
			name:       "policy gone",
			mutate:     func(f *vtsFixture) { f.vtp = nil },
			wantStatus: metav1.ConditionFalse, wantReason: ConditionReasonMissingPolicy,
			wantMessage: "VolumeTierPolicy " + testPolicy + " not found",
		},
		{
			name:       "volume is not CSI",
			mutate:     func(f *vtsFixture) { f.pv.Spec.CSI = nil },
			wantStatus: metav1.ConditionFalse, wantReason: ConditionReasonCannotAdopt, wantMessage: "pv is not a CSI volume",
		},
		{
			name:       "tier class gone",
			mutate:     func(f *vtsFixture) { f.vac = nil },
			wantStatus: metav1.ConditionFalse, wantReason: ConditionReasonCannotAdopt,
			wantMessage: "VolumeAttributesClass " + tierBase + " not found",
		},
		{
			name: "no tier declares ceilings",
			mutate: func(f *vtsFixture) {
				f.vtp.Spec.Tiers = []tierdv1alpha1.Tier{{VACName: tierBase}, {VACName: tierFast}}
			},
			wantStatus: metav1.ConditionFalse, wantReason: ConditionReasonCannotAdopt, wantMessage: "No tier declares ceilings",
		},
		{
			name:       "adopted",
			mutate:     func(*vtsFixture) {},
			wantStatus: metav1.ConditionTrue, wantReason: ConditionReasonPolicyMatched, wantMessage: testPolicy,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVTSFixture()
			tt.mutate(f)
			c := reconcileWith(t, f.objects()...)

			cond := meta.FindStatusCondition(getState(t, c).Status.Conditions, ConditionVTSManaged)
			if cond == nil || cond.Status != tt.wantStatus || cond.Reason != tt.wantReason ||
				!strings.Contains(cond.Message, tt.wantMessage) {
				t.Errorf("Managed = %+v, want %s/%s containing %q", cond, tt.wantStatus, tt.wantReason, tt.wantMessage)
			}
		})
	}
}

func TestReconcileWithoutState(t *testing.T) {
	f := newVTSFixture()
	f.vts = nil
	c, rec := modifyClient(t, interceptor.Funcs{}, f.objects()...)
	r := &VolumeTierStateReconciler{Client: c, Recorder: rec}
	if res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: testKey}); err != nil || !res.IsZero() {
		t.Errorf("Reconcile = %+v, %v; want nothing to do", res, err)
	}
}

func TestReconcileChangesTier(t *testing.T) {
	f := newVTSFixture().onTier(tierBase)
	c, rec := modifyClient(t, interceptor.Funcs{}, f.objects()...)
	r := &VolumeTierStateReconciler{Client: c, Recorder: rec}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: testKey}); err != nil {
		t.Fatal(err)
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(t.Context(), testKey, pvc); err != nil {
		t.Fatal(err)
	}
	if got := ptr.Deref(pvc.Spec.VolumeAttributesClassName, ""); got != tierFast {
		t.Errorf("PVC class = %q, want %s", got, tierFast)
	}
	st := getState(t, c).Status
	if st.LastAppliedTier != tierFast || st.LastModified == nil || st.PendingSince != nil {
		t.Errorf("state = %+v, want fast applied now with no pending change", st)
	}
	if ev := <-rec.Events; !strings.Contains(ev, "TierChanged Changed tier from "+tierBase+" to "+tierFast) {
		t.Errorf("event = %q, want TierChanged from base to fast", ev)
	}
}

// TestReconcileErrors checks that API failures are returned, so the
// reconcile is retried, rather than recorded on the state.
func TestReconcileErrors(t *testing.T) {
	injected := errors.New("injected")
	failGet := func(match func(client.Object) bool) interceptor.Funcs {
		return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption) error {
			if match(obj) {
				return injected
			}
			return c.Get(ctx, key, obj, opts...)
		}}
	}
	is := func(proto client.Object) func(client.Object) bool {
		return func(obj client.Object) bool { return reflect.TypeOf(obj) == reflect.TypeOf(proto) }
	}
	failStatusPatch := interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string,
		client.Object, client.Patch, ...client.SubResourcePatchOption) error {
		return injected
	}}
	failPatch := interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch,
		...client.PatchOption) error {
		return injected
	}}

	tests := []struct {
		name   string
		mutate func(f *vtsFixture)
		funcs  interceptor.Funcs
	}{
		{name: "reading the state", funcs: failGet(is(&tierdv1alpha1.VolumeTierState{}))},
		{name: "reading the PVC", funcs: failGet(is(&corev1.PersistentVolumeClaim{}))},
		{name: "reading the policy", funcs: failGet(is(&tierdv1alpha1.VolumeTierPolicy{}))},
		{name: "reading the sample", funcs: failGet(is(&tierdv1alpha1.VolumeIOSample{}))},
		{name: "reading the PV", funcs: failGet(is(&corev1.PersistentVolume{}))},
		{name: "reading the tier class", funcs: failGet(is(&storagev1.VolumeAttributesClass{}))},
		{name: "recording an invalid PVC", mutate: func(f *vtsFixture) { f.pvc = nil }, funcs: failStatusPatch},
		{name: "recording a missing policy ref", mutate: func(f *vtsFixture) { f.vts.Status.PolicyRef = "" },
			funcs: failStatusPatch},
		{name: "recording a missing policy", mutate: func(f *vtsFixture) { f.vtp = nil }, funcs: failStatusPatch},
		{name: "recording the state", funcs: failStatusPatch},
		{name: "setting the PVC's tier", funcs: failPatch},
		{name: "resuming from a manual override", funcs: failPatch, mutate: func(f *vtsFixture) {
			// The annotation was removed after a hand-picked tier.
			f.onTier(tierFast)
			f.vts.Status.Conditions = []metav1.Condition{{
				Type: ConditionVTSManualOverride, Status: metav1.ConditionTrue, Reason: "Annotated",
			}}
		}},
		{name: "checking the modify", funcs: failGet(is(&storagev1.VolumeAttributesClass{})), mutate: func(f *vtsFixture) {
			// On fast and idle: no change, but the last move to ultra failed.
			f.onTier(tierFast)
			f.sample.Status.IOPS = 0
			f.pvc.Status.ModifyVolumeStatus = &corev1.ModifyVolumeStatus{
				Status: corev1.PersistentVolumeClaimModifyVolumeInfeasible, TargetVolumeAttributesClassName: "ultra",
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVTSFixture()
			if tt.mutate != nil {
				tt.mutate(f)
			}
			c, rec := modifyClient(t, tt.funcs, f.objects()...)
			r := &VolumeTierStateReconciler{Client: c, Recorder: rec}
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: testKey})
			if !errors.Is(err, injected) {
				t.Errorf("Reconcile = %v, want the injected error", err)
			}
		})
	}
}

func TestStatesForPolicyListFails(t *testing.T) {
	c, rec := modifyClient(t, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList,
		...client.ListOption) error {
		return apierrors.NewServiceUnavailable("injected")
	}})
	r := &VolumeTierStateReconciler{Client: c, Recorder: rec}
	if got := r.statesForPolicy(t.Context(), &tierdv1alpha1.VolumeTierPolicy{Namespace: "ns", Name: testPolicy}); got != nil {
		t.Errorf("statesForPolicy on a failing List = %v, want nil", got)
	}
}
