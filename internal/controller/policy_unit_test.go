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
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tierdv1alpha1 "github.com/zanderyk/tierd/api/v1alpha1"
)

func TestValidateTierSpec(t *testing.T) {
	ceil := func(iops, mibps int64) *tierdv1alpha1.Ceiling {
		c := &tierdv1alpha1.Ceiling{}
		if iops > 0 {
			c.IOPS = new(iops)
		}
		if mibps > 0 {
			c.ThroughputMiBPS = new(mibps)
		}
		return c
	}
	tests := []struct {
		name  string
		tiers []tierdv1alpha1.Tier
		want  []string // substrings of the error; none means valid
	}{
		{"ascending", []tierdv1alpha1.Tier{{VACName: "a", Ceilings: ceil(100, 10)}, {VACName: "b", Ceilings: ceil(200, 20)}}, nil},
		{"without ceilings", []tierdv1alpha1.Tier{{VACName: "a"}, {VACName: "b"}}, nil},
		{"metrics compared only where declared",
			[]tierdv1alpha1.Tier{{VACName: "a", Ceilings: ceil(100, 0)}, {VACName: "b", Ceilings: ceil(0, 50)},
				{VACName: "c", Ceilings: ceil(200, 60)}}, nil},
		{"one tier", []tierdv1alpha1.Tier{{VACName: "a"}}, []string{"at least two tiers are required, got 1"}},
		{"unnamed tier", []tierdv1alpha1.Tier{{VACName: "a"}, {}}, []string{"tier 1: name must be set"}},
		{"duplicate name", []tierdv1alpha1.Tier{{VACName: "a"}, {VACName: "a"}}, []string{`tier "a": duplicate name`}},
		{"empty ceilings", []tierdv1alpha1.Tier{{VACName: "a", Ceilings: &tierdv1alpha1.Ceiling{}}, {VACName: "b"}},
			[]string{`tier "a": ceilings must set iops and/or throughputMiBps`}},
		{"iops not increasing", []tierdv1alpha1.Tier{{VACName: "a", Ceilings: ceil(200, 0)}, {VACName: "b", Ceilings: ceil(200, 0)}},
			[]string{`tier "b": iops ceiling 200 must be greater than tier "a" (200)`}},
		{"every problem reported",
			[]tierdv1alpha1.Tier{{VACName: "a", Ceilings: ceil(0, 20)}, {VACName: "b", Ceilings: ceil(0, 10)}, {VACName: "b"}},
			[]string{`tier "b": throughputMiBps ceiling 10 must be greater than tier "a" (20)`, `tier "b": duplicate name`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTierSpec(tt.tiers)
			if len(tt.want) == 0 {
				if err != nil {
					t.Errorf("validateTierSpec = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateTierSpec = nil, want %q", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

// policyClient is a fake API server holding objs, with the tier name index
// SetupWithManager registers.
func policyClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) (client.Client, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := tierdv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&tierdv1alpha1.VolumeTierState{}, &tierdv1alpha1.VolumeTierPolicy{}).
		WithIndex(&tierdv1alpha1.VolumeTierPolicy{}, tierVACNameIndex, func(obj client.Object) []string {
			tiers := obj.(*tierdv1alpha1.VolumeTierPolicy).Spec.Tiers
			names := make([]string, 0, len(tiers))
			for _, tier := range tiers {
				names = append(names, tier.VACName)
			}
			return names
		}).Build()
	return interceptor.NewClient(c, funcs), scheme
}

func TestPoliciesForVAC(t *testing.T) {
	policy := func(ns, name string, tiers ...string) *tierdv1alpha1.VolumeTierPolicy {
		vtp := &tierdv1alpha1.VolumeTierPolicy{Namespace: ns, Name: name}
		for _, tier := range tiers {
			vtp.Spec.Tiers = append(vtp.Spec.Tiers, tierdv1alpha1.Tier{VACName: tier})
		}
		return vtp
	}
	c, _ := policyClient(t, interceptor.Funcs{},
		policy("a", "uses-fast", tierBase, tierFast),
		policy("b", "uses-fast-too", tierFast, "ultra"),
		policy("a", "base-only", tierBase, "slow"))
	r := &VolumeTierPolicyReconciler{Client: c}

	got := r.policiesForVAC(t.Context(), &storagev1.VolumeAttributesClass{Name: tierFast})
	want := []reconcile.Request{
		{Namespace: "a", Name: "uses-fast"},
		{Namespace: "b", Name: "uses-fast-too"},
	}
	sortRequests := func(reqs []reconcile.Request) {
		slices.SortFunc(reqs, func(x, y reconcile.Request) int { return strings.Compare(x.String(), y.String()) })
	}
	sortRequests(got)
	if !slices.Equal(got, want) {
		t.Errorf("policiesForVAC(fast) = %v, want %v", got, want)
	}
	if got := r.policiesForVAC(t.Context(), &storagev1.VolumeAttributesClass{Name: "unused"}); len(got) != 0 {
		t.Errorf("policiesForVAC(unused) = %v, want none", got)
	}

	failing, _ := policyClient(t, interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList,
		...client.ListOption) error {
		return errors.New("injected")
	}})
	r = &VolumeTierPolicyReconciler{Client: failing}
	if got := r.policiesForVAC(t.Context(), &storagev1.VolumeAttributesClass{Name: tierFast}); got != nil {
		t.Errorf("policiesForVAC on a failing List = %v, want nil", got)
	}
}

func TestEnsureVolumeTierState(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{Namespace: testKey.Namespace, Name: testKey.Name, UID: "uid-data"}
	stateGR := schema.GroupResource{Group: tierdv1alpha1.GroupVersion.Group, Resource: "volumetierstates"}
	injected := errors.New("injected")
	isState := func(obj client.Object) bool { _, ok := obj.(*tierdv1alpha1.VolumeTierState); return ok }

	t.Run("creates the state owned by the PVC", func(t *testing.T) {
		c, scheme := policyClient(t, interceptor.Funcs{}, pvc.DeepCopy())
		r := &VolumeTierPolicyReconciler{Client: c, Scheme: scheme}
		state, err := r.ensureVolumeTierState(t.Context(), pvc)
		if err != nil {
			t.Fatal(err)
		}
		if refs := state.OwnerReferences; len(refs) != 1 || refs[0].UID != pvc.UID {
			t.Errorf("owner references = %+v, want the PVC", refs)
		}
	})

	t.Run("fetches a state created concurrently", func(t *testing.T) {
		existing := &tierdv1alpha1.VolumeTierState{Namespace: testKey.Namespace, Name: testKey.Name,
			Status: tierdv1alpha1.VolumeTierStateStatus{PolicyRef: testPolicy}}
		missed := false
		c, scheme := policyClient(t, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch,
			key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if isState(obj) && !missed {
				missed = true
				return apierrors.NewNotFound(stateGR, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		}}, pvc.DeepCopy(), existing)
		r := &VolumeTierPolicyReconciler{Client: c, Scheme: scheme}
		state, err := r.ensureVolumeTierState(t.Context(), pvc)
		if err != nil {
			t.Fatal(err)
		}
		if state.Status.PolicyRef != testPolicy || state.ResourceVersion == "" {
			t.Errorf("state = %+v, want the existing one", state)
		}
	})

	failGetState := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		obj client.Object, opts ...client.GetOption) error {
		if isState(obj) {
			return injected
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	failCreate := interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object,
		...client.CreateOption) error {
		return injected
	}}
	for name, funcs := range map[string]interceptor.Funcs{"get fails": failGetState, "create fails": failCreate} {
		t.Run(name, func(t *testing.T) {
			c, scheme := policyClient(t, funcs, pvc.DeepCopy())
			r := &VolumeTierPolicyReconciler{Client: c, Scheme: scheme}
			if _, err := r.ensureVolumeTierState(t.Context(), pvc); !errors.Is(err, injected) {
				t.Errorf("ensureVolumeTierState = %v, want the injected error", err)
			}
		})
	}

	t.Run("refetch after a concurrent create fails", func(t *testing.T) {
		existing := &tierdv1alpha1.VolumeTierState{Namespace: testKey.Namespace, Name: testKey.Name}
		gets := 0
		c, scheme := policyClient(t, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch,
			key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if isState(obj) {
				if gets++; gets == 1 {
					return apierrors.NewNotFound(stateGR, key.Name)
				}
				return injected
			}
			return c.Get(ctx, key, obj, opts...)
		}}, pvc.DeepCopy(), existing)
		r := &VolumeTierPolicyReconciler{Client: c, Scheme: scheme}
		if _, err := r.ensureVolumeTierState(t.Context(), pvc); !errors.Is(err, injected) {
			t.Errorf("ensureVolumeTierState = %v, want the injected error", err)
		}
	})
}

func TestPVCDriver(t *testing.T) {
	const scDriver, pvDriver = "sc.csi.example.com", "pv.csi.example.com"
	sc := &storagev1.StorageClass{Name: "sc", Provisioner: scDriver}
	csiPV := &corev1.PersistentVolume{Name: "pv", Spec: corev1.PersistentVolumeSpec{
		PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: pvDriver}},
	}}
	hostPV := &corev1.PersistentVolume{Name: "pv", Spec: corev1.PersistentVolumeSpec{
		PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/x"}},
	}}
	pvc := func(class, volume string) *corev1.PersistentVolumeClaim {
		p := &corev1.PersistentVolumeClaim{Namespace: testKey.Namespace, Name: testKey.Name}
		if class != "" {
			p.Spec.StorageClassName = new(class)
		}
		p.Spec.VolumeName = volume
		return p
	}
	tests := []struct {
		name string
		pvc  *corev1.PersistentVolumeClaim
		objs []client.Object
		want string
	}{
		{"from the StorageClass", pvc("sc", "pv"), []client.Object{sc, csiPV}, scDriver},
		{"StorageClass gone: from the PV", pvc("sc", "pv"), []client.Object{csiPV}, pvDriver},
		{"no StorageClass: from the PV", pvc("", "pv"), []client.Object{csiPV}, pvDriver},
		{"not bound yet", pvc("", ""), nil, ""},
		{"PV gone", pvc("", "pv"), nil, ""},
		{"PV is not CSI", pvc("", "pv"), []client.Object{hostPV}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := policyClient(t, interceptor.Funcs{}, tt.objs...)
			r := &VolumeTierPolicyReconciler{Client: c}
			got, err := r.pvcDriver(t.Context(), tt.pvc)
			if err != nil || got != tt.want {
				t.Errorf("pvcDriver = %q, %v; want %q", got, err, tt.want)
			}
		})
	}

	injected := errors.New("injected")
	for name, p := range map[string]*corev1.PersistentVolumeClaim{
		"reading the StorageClass": pvc("sc", ""),
		"reading the PV":           pvc("", "pv"),
	} {
		t.Run(name+" fails", func(t *testing.T) {
			c, _ := policyClient(t, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey,
				client.Object, ...client.GetOption) error {
				return injected
			}})
			r := &VolumeTierPolicyReconciler{Client: c}
			if _, err := r.pvcDriver(t.Context(), p); !errors.Is(err, injected) {
				t.Errorf("pvcDriver = %v, want the injected error", err)
			}
		})
	}
}

func TestNamespacePolicies(t *testing.T) {
	const ns, appLabel = "ns", "app"
	policy := func(name string, sel metav1.LabelSelector, tiers ...string) *tierdv1alpha1.VolumeTierPolicy {
		vtp := &tierdv1alpha1.VolumeTierPolicy{Namespace: ns, Name: name, Spec: tierdv1alpha1.VolumeTierPolicySpec{Selector: sel}}
		for _, tier := range tiers {
			vtp.Spec.Tiers = append(vtp.Spec.Tiers, tierdv1alpha1.Tier{VACName: tier})
		}
		return vtp
	}
	app := metav1.LabelSelector{MatchLabels: map[string]string{appLabel: "db"}}
	invalid := metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: appLabel, Operator: "Bogus"}}}
	objs := []client.Object{
		&storagev1.VolumeAttributesClass{Name: tierBase, DriverName: "d"},
		policy("valid", app, tierBase, tierFast),
		policy("bad-selector", invalid, tierBase, tierFast),
		policy("no-tiers", app),
		policy("class-gone", app, "missing", tierFast),
		policy("elsewhere", app, tierBase, tierFast),
	}
	objs[len(objs)-1].SetNamespace("other")
	self := policy("self", app, tierBase, tierFast)
	own := policyMatch{selector: labels.Everything(), driver: "own"}

	c, _ := policyClient(t, interceptor.Funcs{}, objs...)
	r := &VolumeTierPolicyReconciler{Client: c}
	got, err := r.namespacePolicies(t.Context(), self, own)
	if err != nil {
		t.Fatal(err)
	}
	names := slices.Sorted(maps.Keys(got))
	if !slices.Equal(names, []string{"self", "valid"}) {
		t.Errorf("policies = %v, want self and valid", names)
	}
	if got["valid"].driver != "d" || got["self"].driver != "own" {
		t.Errorf("drivers = valid:%q self:%q, want d and own", got["valid"].driver, got["self"].driver)
	}

	injected := errors.New("injected")
	for name, funcs := range map[string]interceptor.Funcs{
		"listing policies": {List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return injected
		}},
		"reading a tier class": {Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object,
			...client.GetOption) error {
			return injected
		}},
	} {
		t.Run(name+" fails", func(t *testing.T) {
			c, _ := policyClient(t, funcs, objs...)
			r := &VolumeTierPolicyReconciler{Client: c}
			if _, err := r.namespacePolicies(t.Context(), self, own); !errors.Is(err, injected) {
				t.Errorf("namespacePolicies = %v, want the injected error", err)
			}
		})
	}
}
