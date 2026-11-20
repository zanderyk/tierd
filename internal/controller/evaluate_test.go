/*
Copyright 2026.

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
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
)

const (
	tierBase = "base"
	tierFast = "fast"
)

func TestWantDirection(t *testing.T) {
	// Same shape as hack/aks-smoke/policy.yaml.
	spec := tierdv1.VolumeTierPolicySpec{
		Tiers: []tierdv1.Tier{
			{VACName: tierBase, Ceilings: &tierdv1.Ceiling{IOPS: new(int64(3000)), ThroughputMiBPS: new(int64(125))}},
			{VACName: tierFast, Ceilings: &tierdv1.Ceiling{IOPS: new(int64(5000)), ThroughputMiBPS: new(int64(200))}},
		},
		ScaleUp: tierdv1.ScaleUp{
			IOPressureAbove:   new(int32(20)),
			LatencyP95AboveMs: new(int64(20)),
			UtilizationAbove:  new(int32(80)),
		},
		ScaleDown: &tierdv1.ScaleDown{UtilizationBelow: 30, IOPressureBelow: new(int32(1))},
	}
	psi := func(v string) *resource.Quantity { return new(resource.MustParse(v)) }

	tests := []struct {
		name   string
		tier   int
		sample tierdv1.VolumeIOSampleStatus
		want   tierdv1.ScaleDirection
	}{
		{"idle on base stays", 0, tierdv1.VolumeIOSampleStatus{IOPS: 10, IOPressureSomeAvg60: psi("0")}, tierdv1.ScaleDirectionNone},
		{"iops near base ceiling", 0, tierdv1.VolumeIOSampleStatus{IOPS: 3060}, tierdv1.ScaleDirectionUp},
		{"throughput near base ceiling", 0, tierdv1.VolumeIOSampleStatus{ThroughputBytesPerSecond: 110 << 20}, tierdv1.ScaleDirectionUp},
		{"high latency", 0, tierdv1.VolumeIOSampleStatus{LatencyP95Micros: 25_000}, tierdv1.ScaleDirectionUp},
		{"high pressure", 0, tierdv1.VolumeIOSampleStatus{IOPressureSomeAvg60: psi("35")}, tierdv1.ScaleDirectionUp},
		{"busy on top tier stays", 1, tierdv1.VolumeIOSampleStatus{IOPS: 4900}, tierdv1.ScaleDirectionNone},
		{"quiet on fast goes down", 1, tierdv1.VolumeIOSampleStatus{IOPS: 300, IOPressureSomeAvg60: psi("0")}, tierdv1.ScaleDirectionDown},
		{"too busy for base stays", 1, tierdv1.VolumeIOSampleStatus{IOPS: 2000, IOPressureSomeAvg60: psi("0")}, tierdv1.ScaleDirectionNone},
		{"unknown pressure blocks down", 1, tierdv1.VolumeIOSampleStatus{IOPS: 300}, tierdv1.ScaleDirectionNone},
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
	spec := tierdv1.VolumeTierPolicySpec{
		Tiers:     []tierdv1.Tier{{VACName: tierBase}, {VACName: tierFast}},
		ScaleUp:   tierdv1.ScaleUp{For: metav1.Duration{Duration: 10 * time.Minute}},
		ScaleDown: &tierdv1.ScaleDown{For: metav1.Duration{Duration: time.Hour}},
	}
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return start.Add(time.Duration(m) * time.Minute) }

	// One PVC on tierBase, fed a sequence of directions over time.
	st := &tierdv1.VolumeTierStateStatus{CurrentTier: tierBase}
	steps := []struct {
		name      string
		minute    int
		dir       tierdv1.ScaleDirection
		wantTier  string
		wantAfter time.Duration
	}{
		{"load starts: stopwatch starts", 0, tierdv1.ScaleDirectionUp, tierBase, 10 * time.Minute},
		{"still loaded, window not over", 4, tierdv1.ScaleDirectionUp, tierBase, 6 * time.Minute},
		{"load drops: stopwatch resets", 5, tierdv1.ScaleDirectionNone, tierBase, 0},
		{"load returns: starts again", 6, tierdv1.ScaleDirectionUp, tierBase, 10 * time.Minute},
		{"held for the full window", 16, tierdv1.ScaleDirectionUp, tierFast, 0},
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
	spec := tierdv1.VolumeTierPolicySpec{
		Tiers:     []tierdv1.Tier{{VACName: tierBase}, {VACName: tierFast}},
		ScaleUp:   tierdv1.ScaleUp{For: metav1.Duration{Duration: 10 * time.Minute}},
		ScaleDown: &tierdv1.ScaleDown{For: metav1.Duration{Duration: time.Hour}},
	}
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	st := &tierdv1.VolumeTierStateStatus{CurrentTier: tierFast}

	recommend(st, spec, tierdv1.ScaleDirectionDown, "idle", now)
	if after := recommend(st, spec, tierdv1.ScaleDirectionDown, "idle", now.Add(30*time.Minute)); after != 30*time.Minute {
		t.Errorf("after 30m of a 1h window: requeue %v, want 30m", after)
	}
	recommend(st, spec, tierdv1.ScaleDirectionDown, "idle", now.Add(time.Hour))
	if st.RecommendedTier != tierBase {
		t.Errorf("RecommendedTier = %q, want base", st.RecommendedTier)
	}
}

func TestShouldAct(t *testing.T) {
	policy := func(mode tierdv1.VolumeTierPolicyMode) *tierdv1.VolumeTierPolicy {
		return &tierdv1.VolumeTierPolicy{Spec: tierdv1.VolumeTierPolicySpec{Mode: mode}}
	}
	state := func(current, recommended string) *tierdv1.VolumeTierState {
		return &tierdv1.VolumeTierState{Status: tierdv1.VolumeTierStateStatus{
			CurrentTier: current, RecommendedTier: recommended,
		}}
	}
	pvc := func(specVAC string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{
			VolumeAttributesClassName: &specVAC,
		}}
	}
	overridden := func(vts *tierdv1.VolumeTierState) *tierdv1.VolumeTierState {
		vts.Status.Conditions = []metav1.Condition{{Type: ConditionVTSManualOverride, Status: metav1.ConditionTrue}}
		return vts
	}
	act, rec := tierdv1.VolumeTierPolicyModeAct, tierdv1.VolumeTierPolicyModeRecommend

	tests := []struct {
		name   string
		policy *tierdv1.VolumeTierPolicy
		state  *tierdv1.VolumeTierState
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
	act := &tierdv1.VolumeTierPolicy{Spec: tierdv1.VolumeTierPolicySpec{Mode: tierdv1.VolumeTierPolicyModeAct}}
	pvc := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{
		VolumeAttributesClassName: new(tierBase),
	}}
	state := func(retryAfter time.Time) *tierdv1.VolumeTierState {
		return &tierdv1.VolumeTierState{Status: tierdv1.VolumeTierStateStatus{
			CurrentTier:     tierBase,
			RecommendedTier: tierFast,
			InfeasibleTiers: []tierdv1.InfeasibleTier{{Name: tierFast, RetryAfter: metav1.NewTime(retryAfter)}},
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
	st := &tierdv1.VolumeTierStateStatus{LastModified: new(metav1.NewTime(t0))}
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
	st := &tierdv1.VolumeTierStateStatus{}
	now := time.Now()
	markInfeasible(st, tierFast, "rejected", "", nil, now)
	markInfeasible(st, tierFast, "rejected", "", nil, now.Add(time.Minute))
	if got := st.InfeasibleTiers[0].Attempts; got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestRecommendSkipsTierInBackoff(t *testing.T) {
	spec := tierdv1.VolumeTierPolicySpec{
		Tiers:   []tierdv1.Tier{{VACName: tierBase}, {VACName: tierFast}},
		ScaleUp: tierdv1.ScaleUp{For: metav1.Duration{Duration: time.Minute}},
	}
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	st := &tierdv1.VolumeTierStateStatus{
		CurrentTier:     tierBase,
		InfeasibleTiers: []tierdv1.InfeasibleTier{{Name: tierFast, RetryAfter: metav1.NewTime(until)}},
	}

	recommend(st, spec, tierdv1.ScaleDirectionUp, "busy", now)
	after := recommend(st, spec, tierdv1.ScaleDirectionUp, "busy", now.Add(2*time.Minute))
	if st.RecommendedTier != tierBase {
		t.Errorf("during backoff: RecommendedTier = %q, want base", st.RecommendedTier)
	}
	if want := until.Sub(now.Add(2 * time.Minute)); after != want {
		t.Errorf("during backoff: requeue %v, want %v", after, want)
	}

	recommend(st, spec, tierdv1.ScaleDirectionUp, "busy", until)
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
	entry := func(name string, uid string, size string) tierdv1.InfeasibleTier {
		return tierdv1.InfeasibleTier{Name: name, VACUID: types.UID(uid), Capacity: new(resource.MustParse(size))}
	}

	tests := []struct {
		name  string
		entry tierdv1.InfeasibleTier
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
			st := &tierdv1.VolumeTierStateStatus{InfeasibleTiers: []tierdv1.InfeasibleTier{tt.entry}}
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
		status        tierdv1.VolumeTierStateStatus
		wantOverride  bool
		wantAnnotated bool
		wantLast      string
	}{
		{
			name:    "spec matches what tierd set",
			specVAC: tierFast, status: tierdv1.VolumeTierStateStatus{LastAppliedTier: tierFast},
			wantLast: tierFast,
		},
		{
			name:    "tier changed by hand",
			specVAC: tierBase, status: tierdv1.VolumeTierStateStatus{LastAppliedTier: tierFast},
			wantOverride: true, wantAnnotated: true, wantLast: tierFast,
		},
		{
			name:    "tier changed by hand before tierd ever acted",
			specVAC: tierFast, status: tierdv1.VolumeTierStateStatus{CurrentTier: tierBase},
			wantOverride: true, wantAnnotated: true,
		},
		{
			name:    "annotated by the user",
			specVAC: tierBase, annotated: true, status: tierdv1.VolumeTierStateStatus{CurrentTier: tierBase},
			wantOverride: true, wantAnnotated: true,
		},
		{
			name:    "annotation removed resumes from the spec tier",
			specVAC: tierBase, status: tierdv1.VolumeTierStateStatus{LastAppliedTier: tierFast, Conditions: overridden},
			wantLast: tierBase,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pvc := &corev1.PersistentVolumeClaim{
				Name: "data", Namespace: "default",
				Spec: corev1.PersistentVolumeClaimSpec{VolumeAttributesClassName: new(tt.specVAC)},
			}
			if tt.annotated {
				pvc.Annotations = map[string]string{ManualOverrideAnnotation: "true"}
			}
			c := fake.NewClientBuilder().WithObjects(pvc).Build()
			r := &VolumeTierStateReconciler{Client: c, Recorder: events.NewFakeRecorder(10)}
			vts := &tierdv1.VolumeTierState{Status: tt.status}

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
		})
	}
}
