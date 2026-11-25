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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	tierdv1alpha1 "github.com/zanderyk/tierd/api/v1alpha1"
)

const (
	tierBase = "base"
	tierFast = "fast"
)

func TestWantDirection(t *testing.T) {
	// Same shape as hack/aks-smoke/policy.yaml.
	spec := tierdv1alpha1.VolumeTierPolicySpec{
		Tiers: []tierdv1alpha1.Tier{
			{VACName: tierBase, Ceilings: &tierdv1alpha1.Ceiling{IOPS: new(int64(3000)), ThroughputMiBPS: new(int64(125))}},
			{VACName: tierFast, Ceilings: &tierdv1alpha1.Ceiling{IOPS: new(int64(5000)), ThroughputMiBPS: new(int64(200))}},
		},
		ScaleUp: tierdv1alpha1.ScaleUp{
			IOPressureAbove:   new(int32(20)),
			LatencyP95AboveMs: new(int64(20)),
			UtilizationAbove:  new(int32(80)),
		},
		ScaleDown: &tierdv1alpha1.ScaleDown{UtilizationBelow: 30, IOPressureBelow: new(int32(1))},
	}
	psi := func(v string) *resource.Quantity { return new(resource.MustParse(v)) }

	tests := []struct {
		name   string
		tier   int
		sample tierdv1alpha1.VolumeIOSampleStatus
		want   tierdv1alpha1.ScaleDirection
	}{
		{"idle on base stays", 0, tierdv1alpha1.VolumeIOSampleStatus{IOPS: 10, IOPressureSomeAvg60: psi("0")}, tierdv1alpha1.ScaleDirectionNone},
		{"iops near base ceiling", 0, tierdv1alpha1.VolumeIOSampleStatus{IOPS: 3060}, tierdv1alpha1.ScaleDirectionUp},
		{"throughput near base ceiling", 0, tierdv1alpha1.VolumeIOSampleStatus{ThroughputBytesPerSecond: 110 << 20}, tierdv1alpha1.ScaleDirectionUp},
		{"high latency", 0, tierdv1alpha1.VolumeIOSampleStatus{LatencyP95Micros: new(int64(25_000))}, tierdv1alpha1.ScaleDirectionUp},
		{"high pressure", 0, tierdv1alpha1.VolumeIOSampleStatus{IOPressureSomeAvg60: psi("35")}, tierdv1alpha1.ScaleDirectionUp},
		{"busy on top tier stays", 1, tierdv1alpha1.VolumeIOSampleStatus{IOPS: 4900}, tierdv1alpha1.ScaleDirectionNone},
		{"quiet on fast goes down", 1, tierdv1alpha1.VolumeIOSampleStatus{IOPS: 300, IOPressureSomeAvg60: psi("0")}, tierdv1alpha1.ScaleDirectionDown},
		{"too busy for base stays", 1, tierdv1alpha1.VolumeIOSampleStatus{IOPS: 2000, IOPressureSomeAvg60: psi("0")}, tierdv1alpha1.ScaleDirectionNone},
		{"unknown pressure blocks down", 1, tierdv1alpha1.VolumeIOSampleStatus{IOPS: 300}, tierdv1alpha1.ScaleDirectionNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := wantDirection(spec, tt.tier, tt.sample)
			if got != tt.want {
				t.Errorf("got %q (%s), want %q", got, why, tt.want)
			}
		})
	}
}

func TestRecommend(t *testing.T) {
	spec := tierdv1alpha1.VolumeTierPolicySpec{
		Tiers:     []tierdv1alpha1.Tier{{VACName: tierBase}, {VACName: tierFast}},
		ScaleUp:   tierdv1alpha1.ScaleUp{For: metav1.Duration{Duration: 10 * time.Minute}},
		ScaleDown: &tierdv1alpha1.ScaleDown{For: metav1.Duration{Duration: time.Hour}},
	}
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }

	// One PVC on tierBase, fed a sequence of directions over time.
	st := &tierdv1alpha1.VolumeTierStateStatus{CurrentTier: tierBase}
	steps := []struct {
		name      string
		minute    int
		dir       tierdv1alpha1.ScaleDirection
		wantTier  string
		wantAfter time.Duration
	}{
		{"load starts: stopwatch starts", 0, tierdv1alpha1.ScaleDirectionUp, tierBase, 10 * time.Minute},
		{"still loaded, window not over", 4, tierdv1alpha1.ScaleDirectionUp, tierBase, 6 * time.Minute},
		{"load drops: stopwatch resets", 5, tierdv1alpha1.ScaleDirectionNone, tierBase, 0},
		{"load returns: starts again", 6, tierdv1alpha1.ScaleDirectionUp, tierBase, 10 * time.Minute},
		{"held for the full window", 16, tierdv1alpha1.ScaleDirectionUp, tierFast, 0},
	}
	for _, s := range steps {
		after := recommend(st, spec, s.dir, "busy", at(s.minute))
		if st.RecommendedTier != s.wantTier || after != s.wantAfter {
			t.Fatalf("%s: got tier %q, requeue %v; want %q, %v",
				s.name, st.RecommendedTier, after, s.wantTier, s.wantAfter)
		}
	}
	if st.Reason != "busy for 10m0s" {
		t.Errorf("Reason = %q", st.Reason)
	}
}

func TestRecommendDownUsesScaleDownWindow(t *testing.T) {
	spec := tierdv1alpha1.VolumeTierPolicySpec{
		Tiers:     []tierdv1alpha1.Tier{{VACName: tierBase}, {VACName: tierFast}},
		ScaleUp:   tierdv1alpha1.ScaleUp{For: metav1.Duration{Duration: 10 * time.Minute}},
		ScaleDown: &tierdv1alpha1.ScaleDown{For: metav1.Duration{Duration: time.Hour}},
	}
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	st := &tierdv1alpha1.VolumeTierStateStatus{CurrentTier: tierFast}

	recommend(st, spec, tierdv1alpha1.ScaleDirectionDown, "idle", now)
	if after := recommend(st, spec, tierdv1alpha1.ScaleDirectionDown, "idle", now.Add(30*time.Minute)); after != 30*time.Minute {
		t.Errorf("after 30m of a 1h window: requeue %v, want 30m", after)
	}
	recommend(st, spec, tierdv1alpha1.ScaleDirectionDown, "idle", now.Add(time.Hour))
	if st.RecommendedTier != tierBase {
		t.Errorf("RecommendedTier = %q, want base", st.RecommendedTier)
	}
}

func TestShouldAct(t *testing.T) {
	policy := func(mode tierdv1alpha1.VolumeTierPolicyMode) *tierdv1alpha1.VolumeTierPolicy {
		return &tierdv1alpha1.VolumeTierPolicy{Spec: tierdv1alpha1.VolumeTierPolicySpec{Mode: mode}}
	}
	state := func(current, recommended string) *tierdv1alpha1.VolumeTierState {
		return &tierdv1alpha1.VolumeTierState{Status: tierdv1alpha1.VolumeTierStateStatus{
			CurrentTier: current, RecommendedTier: recommended,
		}}
	}
	pvc := func(specVAC string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{
			VolumeAttributesClassName: &specVAC,
		}}
	}
	overridden := func(vts *tierdv1alpha1.VolumeTierState) *tierdv1alpha1.VolumeTierState {
		vts.Status.Conditions = []metav1.Condition{{Type: ConditionVTSManualOverride, Status: metav1.ConditionTrue}}
		return vts
	}
	act, rec := tierdv1alpha1.VolumeTierPolicyModeAct, tierdv1alpha1.VolumeTierPolicyModeRecommend

	tests := []struct {
		name   string
		policy *tierdv1alpha1.VolumeTierPolicy
		state  *tierdv1alpha1.VolumeTierState
		pvc    *corev1.PersistentVolumeClaim
		want   bool
	}{
		{"act mode with a new recommendation", policy(act), state(tierBase, tierFast), pvc(tierBase), true},
		{"recommend mode never acts", policy(rec), state(tierBase, tierFast), pvc(tierBase), false},
		{"recommendation equals current tier", policy(act), state(tierBase, tierBase), pvc(tierBase), false},
		{"no recommendation yet", policy(act), state(tierBase, ""), pvc(tierBase), false},
		{"change already in flight", policy(act), state(tierBase, tierFast), pvc(tierFast), false},
		{"manual override", policy(act), overridden(state(tierBase, tierFast)), pvc(tierBase), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAct(tt.policy, tt.state, tt.pvc); got != tt.want {
				t.Errorf("shouldAct = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldActWaitsOutInfeasibleBackoff(t *testing.T) {
	act := &tierdv1alpha1.VolumeTierPolicy{Spec: tierdv1alpha1.VolumeTierPolicySpec{Mode: tierdv1alpha1.VolumeTierPolicyModeAct}}
	pvc := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{
		VolumeAttributesClassName: new(tierBase),
	}}
	state := func(retryAfter time.Time) *tierdv1alpha1.VolumeTierState {
		return &tierdv1alpha1.VolumeTierState{Status: tierdv1alpha1.VolumeTierStateStatus{
			CurrentTier:     tierBase,
			RecommendedTier: tierFast,
			InfeasibleTiers: []tierdv1alpha1.InfeasibleTier{{Name: tierFast, RetryAfter: metav1.NewTime(retryAfter)}},
		}}
	}

	if shouldAct(act, state(time.Now().Add(time.Hour)), pvc) {
		t.Error("shouldAct = true during backoff, want false")
	}
	if !shouldAct(act, state(time.Now().Add(-time.Minute)), pvc) {
		t.Error("shouldAct = false after backoff, want true")
	}
}

func TestMarkInfeasible(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := &tierdv1alpha1.VolumeTierStateStatus{LastModified: new(metav1.NewTime(t0))}
	check := func(step string, attempts int32, retryAfter time.Time) {
		t.Helper()
		if len(st.InfeasibleTiers) != 1 {
			t.Fatalf("%s: %d entries, want 1", step, len(st.InfeasibleTiers))
		}
		e := st.InfeasibleTiers[0]
		if e.Attempts != attempts || !e.RetryAfter.Time.Equal(retryAfter) {
			t.Errorf("%s: attempts=%d retryAfter=%s, want %d %s", step, e.Attempts, e.RetryAfter.Time, attempts, retryAfter)
		}
	}

	t1 := t0.Add(time.Minute)
	markInfeasible(st, tierFast, "rejected", "", nil, t1)
	check("first failure", 1, t1.Add(30*time.Minute))

	markInfeasible(st, tierFast, "rejected", "", nil, t1.Add(time.Minute))
	check("same failure seen again", 1, t1.Add(30*time.Minute))

	t2 := t1.Add(time.Hour)
	st.LastModified = new(metav1.NewTime(t2)) // tierd retried
	t3 := t2.Add(time.Minute)
	markInfeasible(st, tierFast, "rejected", "", nil, t3)
	check("second attempt", 2, t3.Add(time.Hour))

	st.InfeasibleTiers[0].Attempts = 20
	t4 := t3.Add(time.Hour)
	st.LastModified = new(metav1.NewTime(t4))
	markInfeasible(st, tierFast, "rejected", "", nil, t4.Add(time.Minute))
	check("capped", 21, t4.Add(time.Minute+6*time.Hour))
}

func TestMarkInfeasibleWithoutLastModified(t *testing.T) {
	// The tier was changed by hand, so tierd never set LastModified.
	st := &tierdv1alpha1.VolumeTierStateStatus{}
	now := time.Now()
	markInfeasible(st, tierFast, "rejected", "", nil, now)
	markInfeasible(st, tierFast, "rejected", "", nil, now.Add(time.Minute))
	if got := st.InfeasibleTiers[0].Attempts; got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestRecommendSkipsTierInBackoff(t *testing.T) {
	spec := tierdv1alpha1.VolumeTierPolicySpec{
		Tiers:   []tierdv1alpha1.Tier{{VACName: tierBase}, {VACName: tierFast}},
		ScaleUp: tierdv1alpha1.ScaleUp{For: metav1.Duration{Duration: time.Minute}},
	}
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	st := &tierdv1alpha1.VolumeTierStateStatus{
		CurrentTier:     tierBase,
		InfeasibleTiers: []tierdv1alpha1.InfeasibleTier{{Name: tierFast, RetryAfter: metav1.NewTime(until)}},
	}

	recommend(st, spec, tierdv1alpha1.ScaleDirectionUp, "busy", now)
	after := recommend(st, spec, tierdv1alpha1.ScaleDirectionUp, "busy", now.Add(2*time.Minute))
	if st.RecommendedTier != tierBase {
		t.Errorf("during backoff: RecommendedTier = %q, want base", st.RecommendedTier)
	}
	if want := until.Sub(now.Add(2 * time.Minute)); after != want {
		t.Errorf("during backoff: requeue %v, want %v", after, want)
	}

	recommend(st, spec, tierdv1alpha1.ScaleDirectionUp, "busy", until)
	if st.RecommendedTier != tierFast {
		t.Errorf("after backoff: RecommendedTier = %q, want fast", st.RecommendedTier)
	}
}

func TestPruneInfeasible(t *testing.T) {
	vac := &storagev1.VolumeAttributesClass{Name: tierFast, UID: "uid-1"}
	r := &VolumeTierStateReconciler{Client: fake.NewClientBuilder().WithObjects(vac).Build()}
	pvc := func(size string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{Status: corev1.PersistentVolumeClaimStatus{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
		}}
	}
	entry := func(name string, uid string, size string) tierdv1alpha1.InfeasibleTier {
		return tierdv1alpha1.InfeasibleTier{Name: name, VACUID: types.UID(uid), Capacity: new(resource.MustParse(size))}
	}

	tests := []struct {
		name  string
		entry tierdv1alpha1.InfeasibleTier
		pvc   *corev1.PersistentVolumeClaim
		keep  bool
	}{
		{"nothing changed", entry(tierFast, "uid-1", "10Gi"), pvc("10Gi"), true},
		{"class deleted", entry("gone", "uid-1", "10Gi"), pvc("10Gi"), false},
		{"class recreated", entry(tierFast, "uid-0", "10Gi"), pvc("10Gi"), false},
		{"PVC grew", entry(tierFast, "uid-1", "10Gi"), pvc("20Gi"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &tierdv1alpha1.VolumeTierStateStatus{InfeasibleTiers: []tierdv1alpha1.InfeasibleTier{tt.entry}}
			if err := r.pruneInfeasible(t.Context(), st, tt.pvc); err != nil {
				t.Fatal(err)
			}
			if kept := len(st.InfeasibleTiers) == 1; kept != tt.keep {
				t.Errorf("kept = %v, want %v", kept, tt.keep)
			}
		})
	}
}

func TestSyncOverride(t *testing.T) {
	overridden := []metav1.Condition{{Type: ConditionVTSManualOverride, Status: metav1.ConditionTrue}}

	tests := []struct {
		name          string
		specVAC       string
		annotated     bool
		applied       string // the PVC's applied-tier annotation
		status        tierdv1alpha1.VolumeTierStateStatus
		wantOverride  bool
		wantAnnotated bool
		wantLast      string
	}{
		{
			name:    "spec matches what tierd set",
			specVAC: tierFast, status: tierdv1alpha1.VolumeTierStateStatus{LastAppliedTier: tierFast},
			wantLast: tierFast,
		},
		{
			name:    "tier changed by hand",
			specVAC: tierBase, status: tierdv1alpha1.VolumeTierStateStatus{LastAppliedTier: tierFast},
			wantOverride: true, wantAnnotated: true, wantLast: tierFast,
		},
		{
			// The PVC patch re-triggers reconcile before the state's cache
			// has tierd's LastAppliedTier.
			name:    "tierd's own change read with a stale state",
			specVAC: tierFast, applied: tierFast, status: tierdv1alpha1.VolumeTierStateStatus{CurrentTier: tierBase, LastAppliedTier: tierBase},
			wantLast: tierBase,
		},
		{
			name:    "tier changed by hand after tierd set it",
			specVAC: tierBase, applied: tierFast, status: tierdv1alpha1.VolumeTierStateStatus{CurrentTier: tierFast},
			wantOverride: true, wantAnnotated: true,
		},
		{
			name:    "tier changed by hand before tierd ever acted",
			specVAC: tierFast, status: tierdv1alpha1.VolumeTierStateStatus{CurrentTier: tierBase},
			wantOverride: true, wantAnnotated: true,
		},
		{
			name:    "annotated by the user",
			specVAC: tierBase, annotated: true, status: tierdv1alpha1.VolumeTierStateStatus{CurrentTier: tierBase},
			wantOverride: true, wantAnnotated: true,
		},
		{
			name:    "annotation removed resumes from the spec tier with a fresh window",
			specVAC: tierBase,
			status: tierdv1alpha1.VolumeTierStateStatus{
				LastAppliedTier: tierFast, Conditions: overridden,
				PendingDirection: tierdv1alpha1.ScaleDirectionUp, PendingSince: new(metav1.Now()),
			},
			wantLast: tierBase,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pvc := &corev1.PersistentVolumeClaim{
				Name: "data", Namespace: "default",
				Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: new(tt.specVAC)},
			}
			pvc.Annotations = map[string]string{}
			if tt.annotated {
				pvc.Annotations[ManualOverrideAnnotation] = "true"
			}
			if tt.applied != "" {
				pvc.Annotations[AppliedTierAnnotation] = tt.applied
			}
			c := fake.NewClientBuilder().WithObjects(pvc).Build()
			r := &VolumeTierStateReconciler{Client: c, Recorder: events.NewFakeRecorder(10)}
			vts := &tierdv1alpha1.VolumeTierState{Status: tt.status}

			if err := r.syncOverride(t.Context(), vts, pvc); err != nil {
				t.Fatal(err)
			}

			if got := meta.IsStatusConditionTrue(vts.Status.Conditions, ConditionVTSManualOverride); got != tt.wantOverride {
				t.Errorf("ManualOverride = %v, want %v", got, tt.wantOverride)
			}
			stored := &corev1.PersistentVolumeClaim{}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(pvc), stored); err != nil {
				t.Fatal(err)
			}
			if _, got := stored.Annotations[ManualOverrideAnnotation]; got != tt.wantAnnotated {
				t.Errorf("annotated = %v, want %v", got, tt.wantAnnotated)
			}
			if vts.Status.LastAppliedTier != tt.wantLast {
				t.Errorf("LastAppliedTier = %q, want %q", vts.Status.LastAppliedTier, tt.wantLast)
			}
			if resumed := meta.FindStatusCondition(vts.Status.Conditions, ConditionVTSManualOverride).Reason == "Resumed"; resumed &&
				stored.Annotations[AppliedTierAnnotation] != tt.specVAC {
				t.Errorf("applied-tier annotation = %q after resume, want %q", stored.Annotations[AppliedTierAnnotation], tt.specVAC)
			}
			if tt.status.PendingSince != nil && (vts.Status.PendingSince != nil || vts.Status.PendingDirection != tierdv1alpha1.ScaleDirectionNone) {
				t.Errorf("pending = %s since %v, want cleared on resume", vts.Status.PendingDirection, vts.Status.PendingSince)
			}
		})
	}
}

// testPVC is the PVC (and state and sample) the reconcile tests work on.
const (
	testPVC    = "data"
	testPolicy = "policy"
)

var testKey = types.NamespacedName{Namespace: "ns", Name: testPVC}

// reconcileWith reconciles testKey once against a fake client holding objs
// and returns the client.
func reconcileWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := tierdv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&tierdv1alpha1.VolumeTierState{}).Build()
	r := &VolumeTierStateReconciler{Client: c, Recorder: events.NewFakeRecorder(10)}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: testKey}); err != nil {
		t.Fatal(err)
	}
	return c
}

// getState returns testKey's VolumeTierState.
func getState(t *testing.T, c client.Client) *tierdv1alpha1.VolumeTierState {
	t.Helper()
	got := &tierdv1alpha1.VolumeTierState{}
	if err := c.Get(t.Context(), testKey, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// reconcileOnce reconciles a bound PVC whose state points at an existing
// policy, starting from a stale MissingPolicy condition, and returns the
// state's Managed condition.
func reconcileOnce(t *testing.T, extra ...client.Object) *metav1.Condition {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		Namespace: testKey.Namespace, Name: testKey.Name,
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	vtp := &tierdv1alpha1.VolumeTierPolicy{Namespace: testKey.Namespace, Name: testPolicy}
	// A stale condition from before the policy controller set policyRef.
	vts := &tierdv1alpha1.VolumeTierState{
		Namespace: testKey.Namespace, Name: testKey.Name,
		Status: tierdv1alpha1.VolumeTierStateStatus{
			PolicyRef: testPolicy,
			Conditions: []metav1.Condition{{
				Type: ConditionVTSManaged, Status: metav1.ConditionFalse,
				Reason: ConditionReasonMissingPolicy, Message: "policy ref is empty",
			}},
		},
	}
	c := reconcileWith(t, append([]client.Object{pvc, vtp, vts}, extra...)...)
	return meta.FindStatusCondition(getState(t, c).Status.Conditions, ConditionVTSManaged)
}

func TestReconcileWithoutSample(t *testing.T) {
	cond := reconcileOnce(t)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ConditionReasonAwaitingSample {
		t.Errorf("Managed = %+v, want False/%s", cond, ConditionReasonAwaitingSample)
	}
}

func TestReconcileUnmeasurableVolume(t *testing.T) {
	const msg = "tierd cannot measure fuse.blobfuse2 volumes"
	sample := &tierdv1alpha1.VolumeIOSample{
		Namespace: "ns", Name: "data",
		Status: tierdv1alpha1.VolumeIOSampleStatus{Conditions: []metav1.Condition{{
			Type: tierdv1alpha1.VolumeIOSampleMeasured, Status: metav1.ConditionFalse,
			Reason: tierdv1alpha1.MeasuredNoSource, Message: msg,
		}}},
	}
	cond := reconcileOnce(t, sample)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tierdv1alpha1.MeasuredNoSource || cond.Message != msg {
		t.Errorf("Managed = %+v, want False/%s %q", cond, tierdv1alpha1.MeasuredNoSource, msg)
	}
}

func TestPickTier(t *testing.T) {
	tier := func(name string, iops int64) tierdv1alpha1.Tier {
		return tierdv1alpha1.Tier{VACName: name, Ceilings: &tierdv1alpha1.Ceiling{IOPS: new(iops)}}
	}
	spec := tierdv1alpha1.VolumeTierPolicySpec{
		Tiers:   []tierdv1alpha1.Tier{tier(tierBase, 1000), tier(tierFast, 5000), tier("ultra", 20000)},
		ScaleUp: tierdv1alpha1.ScaleUp{UtilizationAbove: new(int32(80))},
	}
	noCeilings := tierdv1alpha1.VolumeTierPolicySpec{Tiers: []tierdv1alpha1.Tier{{VACName: "a"}, {VACName: "b"}}}
	topOnly := tierdv1alpha1.VolumeTierPolicySpec{Tiers: []tierdv1alpha1.Tier{{VACName: "a"}, tier("b", 1000)}}

	tests := []struct {
		name   string
		spec   tierdv1alpha1.VolumeTierPolicySpec
		iops   int64
		want   string
		wantOK bool
	}{
		{"fits the lowest tier", spec, 500, tierBase, true},
		{"over the lowest tier's threshold", spec, 852, tierFast, true},
		{"exactly at the threshold moves up", spec, 800, tierFast, true},
		{"exceeds every tier", spec, 30000, "ultra", true},
		{"no threshold means 100%", tierdv1alpha1.VolumeTierPolicySpec{Tiers: spec.Tiers}, 999, tierBase, true},
		{"tiers without ceilings are skipped", topOnly, 10, "b", true},
		{"no tier has ceilings", noCeilings, 10, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why, ok := pickTier(tt.spec, tierdv1alpha1.VolumeIOSampleStatus{IOPS: tt.iops})
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("pickTier = %q, %v (%s); want %q, %v", got, ok, why, tt.want, tt.wantOK)
			}
		})
	}
}

func TestReconcileAdoptsPVCWithoutClass(t *testing.T) {
	const driver = "disk.csi.azure.com"
	objects := func(mode tierdv1alpha1.VolumeTierPolicyMode, pvDriver string) []client.Object {
		pvc := &corev1.PersistentVolumeClaim{
			Namespace: testKey.Namespace, Name: testKey.Name,
			Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: "pv"},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}
		pv := &corev1.PersistentVolume{
			Name: "pv",
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: pvDriver},
			}},
		}
		vtp := &tierdv1alpha1.VolumeTierPolicy{
			Namespace: testKey.Namespace, Name: testPolicy,
			Spec: tierdv1alpha1.VolumeTierPolicySpec{
				Mode: mode,
				Tiers: []tierdv1alpha1.Tier{
					{VACName: tierBase, Ceilings: &tierdv1alpha1.Ceiling{IOPS: new(int64(1000))}},
					{VACName: tierFast, Ceilings: &tierdv1alpha1.Ceiling{IOPS: new(int64(5000))}},
				},
				ScaleUp: tierdv1alpha1.ScaleUp{UtilizationAbove: new(int32(80))},
			},
		}
		vac := &storagev1.VolumeAttributesClass{Name: tierBase, DriverName: driver}
		vts := &tierdv1alpha1.VolumeTierState{
			Namespace: testKey.Namespace, Name: testKey.Name,
			Status: tierdv1alpha1.VolumeTierStateStatus{PolicyRef: testPolicy},
		}
		// 852 IOPS is 85% of base, over the 80% threshold: fast fits.
		sample := &tierdv1alpha1.VolumeIOSample{
			Namespace: testKey.Namespace, Name: testKey.Name,
			Status: tierdv1alpha1.VolumeIOSampleStatus{IOPS: 852},
		}
		return []client.Object{pvc, pv, vtp, vac, vts, sample}
	}
	pvcClass := func(t *testing.T, c client.Client) string {
		t.Helper()
		pvc := &corev1.PersistentVolumeClaim{}
		if err := c.Get(t.Context(), testKey, pvc); err != nil {
			t.Fatal(err)
		}
		return ptr.Deref(pvc.Spec.VolumeAttributesClassName, "")
	}

	t.Run("act sets the tier that fits the load", func(t *testing.T) {
		c := reconcileWith(t, objects(tierdv1alpha1.VolumeTierPolicyModeAct, driver)...)
		if got := pvcClass(t, c); got != tierFast {
			t.Errorf("PVC class = %q, want fast", got)
		}
		if st := getState(t, c).Status; st.RecommendedTier != tierFast || st.LastAppliedTier != tierFast {
			t.Errorf("state = %+v, want fast recommended and applied", st)
		}
	})

	t.Run("recommend only recommends", func(t *testing.T) {
		c := reconcileWith(t, objects(tierdv1alpha1.VolumeTierPolicyModeRecommend, driver)...)
		if got := pvcClass(t, c); got != "" {
			t.Errorf("PVC class = %q, want unset", got)
		}
		if st := getState(t, c).Status; st.RecommendedTier != tierFast {
			t.Errorf("RecommendedTier = %q, want fast", st.RecommendedTier)
		}
	})

	t.Run("another driver's volume is not adopted", func(t *testing.T) {
		c := reconcileWith(t, objects(tierdv1alpha1.VolumeTierPolicyModeAct, "file.csi.azure.com")...)
		if got := pvcClass(t, c); got != "" {
			t.Errorf("PVC class = %q, want unset", got)
		}
		cond := meta.FindStatusCondition(getState(t, c).Status.Conditions, ConditionVTSManaged)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ConditionReasonCannotAdopt {
			t.Errorf("Managed = %+v, want False/%s", cond, ConditionReasonCannotAdopt)
		}
	})
}

func TestReconcileTierNotInPolicy(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		Namespace: testKey.Namespace, Name: testKey.Name,
		Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: new("other")},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound, CurrentVolumeAttributesClassName: new("other"),
		},
	}
	sample := &tierdv1alpha1.VolumeIOSample{Namespace: testKey.Namespace, Name: testKey.Name}
	vtp := &tierdv1alpha1.VolumeTierPolicy{
		Namespace: testKey.Namespace, Name: testPolicy,
		Spec: tierdv1alpha1.VolumeTierPolicySpec{Tiers: []tierdv1alpha1.Tier{{VACName: tierBase}, {VACName: tierFast}}},
	}
	vts := &tierdv1alpha1.VolumeTierState{
		Namespace: testKey.Namespace, Name: testKey.Name,
		Status: tierdv1alpha1.VolumeTierStateStatus{PolicyRef: testPolicy},
	}
	c := reconcileWith(t, pvc, sample, vtp, vts)
	cond := meta.FindStatusCondition(getState(t, c).Status.Conditions, ConditionVTSManaged)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ConditionReasonTierNotInPolicy {
		t.Errorf("Managed = %+v, want False/%s", cond, ConditionReasonTierNotInPolicy)
	}
}
