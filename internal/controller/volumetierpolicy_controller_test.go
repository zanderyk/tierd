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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	zanderykgithubiov1 "github.com/zanderyk/tierd/api/v1"
)

var _ = Describe("VolumeTierPolicy Controller", func() {
	const (
		namespace = "default"
		driver    = "test.csi.example.com"
		vacLow    = "test-tier-low"
		vacHigh   = "test-tier-high"
		pvcName   = "test-pvc"
		policyA   = "policy-a"
		policyB   = "policy-b"
		scName    = "test-sc"

		otherDriver = "other.csi.example.com"
		otherLow    = "other-tier-low"
		otherHigh   = "other-tier-high"

		// vacParam is the parameter that makes each test class distinct.
		vacParam = "tier"
	)
	pvcLabels := map[string]string{"app": "test-db"}

	ctx := context.Background()

	newPolicy := func(name string) *zanderykgithubiov1.VolumeTierPolicy {
		return &zanderykgithubiov1.VolumeTierPolicy{
			Name:      name,
			Namespace: namespace,
			Spec: zanderykgithubiov1.VolumeTierPolicySpec{
				Selector: metav1.LabelSelector{MatchLabels: pvcLabels},
				Mode:     zanderykgithubiov1.VolumeTierPolicyMode("Recommend"),
				Tiers:    []zanderykgithubiov1.Tier{{VACName: vacLow}, {VACName: vacHigh}},
				ScaleUp:  zanderykgithubiov1.ScaleUp{IOPressureAbove: ptr.To[int32](20)},
			},
		}
	}

	reconcileVTP := func(name string) reconcile.Result {
		r := &VolumeTierPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		res, err := r.Reconcile(ctx, reconcile.Request{Namespace: namespace, Name: name})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	getPolicy := func(name string) *zanderykgithubiov1.VolumeTierPolicy {
		vtp := &zanderykgithubiov1.VolumeTierPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, vtp)).To(Succeed())
		return vtp
	}

	getState := func() *zanderykgithubiov1.VolumeTierState {
		state := &zanderykgithubiov1.VolumeTierState{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pvcName}, state)).To(Succeed())
		return state
	}

	deleteIgnoreNotFound := func(obj client.Object) {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
	}

	// forceDelete removes obj and waits until it is gone. envtest runs no
	// controllers, so finalizers (pvc-protection, vac-protection, and the
	// policy finalizer) are stripped by hand.
	forceDelete := func(obj client.Object) {
		key := client.ObjectKeyFromObject(obj)
		err := k8sClient.Get(ctx, key, obj)
		if errors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred())

		obj.SetFinalizers(nil)
		Expect(k8sClient.Update(ctx, obj)).To(Succeed())
		deleteIgnoreNotFound(obj)
		Eventually(func() bool {
			return errors.IsNotFound(k8sClient.Get(ctx, key, obj))
		}).Should(BeTrue())
	}

	BeforeEach(func() {
		By("creating the tier VolumeAttributesClasses, a StorageClass and a labelled PVC")
		Expect(k8sClient.Create(ctx, &storagev1.StorageClass{Name: scName, Provisioner: driver})).To(Succeed())
		for _, name := range []string{vacLow, vacHigh} {
			Expect(k8sClient.Create(ctx, &storagev1.VolumeAttributesClass{
				Name:       name,
				DriverName: driver,
				Parameters: map[string]string{vacParam: name},
			})).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &corev1.PersistentVolumeClaim{
			Name:      pvcName,
			Namespace: namespace,
			Labels:    pvcLabels,
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: ptr.To(scName),
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		By("cleaning up policies, state, PVC and VolumeAttributesClasses")
		policies := &zanderykgithubiov1.VolumeTierPolicyList{}
		Expect(k8sClient.List(ctx, policies, client.InNamespace(namespace))).To(Succeed())
		for i := range policies.Items {
			forceDelete(&policies.Items[i])
		}
		deleteIgnoreNotFound(&zanderykgithubiov1.VolumeTierState{Name: pvcName, Namespace: namespace})

		forceDelete(&corev1.PersistentVolumeClaim{Name: pvcName, Namespace: namespace})
		for _, name := range []string{vacLow, vacHigh, otherLow, otherHigh} {
			forceDelete(&storagev1.VolumeAttributesClass{Name: name})
		}
		deleteIgnoreNotFound(&storagev1.StorageClass{Name: scName})
	})

	// newOtherPolicy is a policy with the same selector whose tiers use
	// otherDriver.
	newOtherPolicy := func(name string) *zanderykgithubiov1.VolumeTierPolicy {
		for _, vac := range []string{otherLow, otherHigh} {
			Expect(k8sClient.Create(ctx, &storagev1.VolumeAttributesClass{
				Name:       vac,
				DriverName: otherDriver,
				Parameters: map[string]string{vacParam: vac},
			})).To(Succeed())
		}
		vtp := newPolicy(name)
		vtp.Spec.Tiers = []zanderykgithubiov1.Tier{{VACName: otherLow}, {VACName: otherHigh}}
		return vtp
	}

	It("skips PVCs of another driver and says so on Ready", func() {
		Expect(k8sClient.Create(ctx, newOtherPolicy(policyB))).To(Succeed())
		reconcileVTP(policyB)

		vtp := getPolicy(policyB)
		Expect(vtp.Status.PVCs).To(BeEmpty())
		cond := meta.FindStatusCondition(vtp.Status.Conditions, string(ConditionVTPReady))
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Message).To(ContainSubstring("Skipped PVCs of other drivers: %s (%s)", pvcName, driver))
		Expect(errors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: namespace, Name: pvcName}, &zanderykgithubiov1.VolumeTierState{}))).To(BeTrue())
	})

	It("does not report Conflict for policies of different drivers with the same selector", func() {
		Expect(k8sClient.Create(ctx, newPolicy(policyA))).To(Succeed())
		Expect(k8sClient.Create(ctx, newOtherPolicy(policyB))).To(Succeed())
		reconcileVTP(policyB)
		reconcileVTP(policyA)

		Expect(getState().Status.PolicyRef).To(Equal(policyA))
		for _, name := range []string{policyA, policyB} {
			Expect(meta.IsStatusConditionFalse(getPolicy(name).Status.Conditions, string(ConditionVTPConflict))).To(BeTrue())
		}
	})

	It("reads the driver from the bound PV when the PVC has no StorageClass", func() {
		r := &VolumeTierPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		pv := &corev1.PersistentVolume{
			Name: "test-pv",
			Spec: corev1.PersistentVolumeSpec{
				Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: otherDriver, VolumeHandle: "h"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pv)).To(Succeed())
		DeferCleanup(forceDelete, pv)

		static := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: new(""),
			VolumeName:       pv.Name,
		}}
		Expect(r.pvcDriver(ctx, static)).To(Equal(otherDriver))

		static.Spec.VolumeName = ""
		Expect(r.pvcDriver(ctx, static)).To(BeEmpty())
	})

	It("assigns the PVC to the only matching policy", func() {
		Expect(k8sClient.Create(ctx, newPolicy(policyA))).To(Succeed())

		res := reconcileVTP(policyA)
		Expect(res.RequeueAfter).To(Equal(membershipResyncInterval))

		vtp := getPolicy(policyA)
		Expect(meta.IsStatusConditionTrue(vtp.Status.Conditions, string(ConditionVTPReady))).To(BeTrue())
		Expect(meta.IsStatusConditionFalse(vtp.Status.Conditions, string(ConditionVTPConflict))).To(BeTrue())
		Expect(vtp.Status.PVCs).To(Equal([]string{pvcName}))

		state := getState()
		Expect(state.Status.PolicyRef).To(Equal(policyA))
		Expect(state.OwnerReferences).To(HaveLen(1))
		Expect(state.OwnerReferences[0].Name).To(Equal(pvcName))
	})

	It("leaves the PVC unmanaged and reports Conflict when two policies match it", func() {
		Expect(k8sClient.Create(ctx, newPolicy(policyA))).To(Succeed())
		reconcileVTP(policyA)
		Expect(getState().Status.PolicyRef).To(Equal(policyA))

		By("adding a second policy with an overlapping selector")
		Expect(k8sClient.Create(ctx, newPolicy(policyB))).To(Succeed())
		reconcileVTP(policyB)
		reconcileVTP(policyA)

		Expect(getState().Status.PolicyRef).To(BeEmpty())
		for _, name := range []string{policyA, policyB} {
			cond := meta.FindStatusCondition(getPolicy(name).Status.Conditions, string(ConditionVTPConflict))
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(string(VTPConditionReasonPolicyConflict)))
			Expect(cond.Message).To(ContainSubstring("%s (policy-a, policy-b)", pvcName))
		}

		By("deleting one policy; the survivor claims the PVC on its next reconcile")
		Expect(k8sClient.Delete(ctx, getPolicy(policyB))).To(Succeed())
		reconcileVTP(policyB) // runs the finalizer, which lets the delete finish
		Expect(errors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: namespace, Name: policyB}, &zanderykgithubiov1.VolumeTierPolicy{}))).To(BeTrue())
		reconcileVTP(policyA)

		Expect(getState().Status.PolicyRef).To(Equal(policyA))
		Expect(meta.IsStatusConditionFalse(getPolicy(policyA).Status.Conditions, string(ConditionVTPConflict))).To(BeTrue())
	})

	stateGone := func() bool {
		return errors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: namespace, Name: pvcName}, &zanderykgithubiov1.VolumeTierState{}))
	}

	It("deletes the state when the PVC stops matching", func() {
		Expect(k8sClient.Create(ctx, newPolicy(policyA))).To(Succeed())
		reconcileVTP(policyA)
		Expect(getState().Status.PolicyRef).To(Equal(policyA))

		By("removing the PVC's label")
		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pvcName}, pvc)).To(Succeed())
		pvc.Labels = nil
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		reconcileVTP(policyA)

		Expect(stateGone()).To(BeTrue())
		Expect(getPolicy(policyA).Status.PVCs).To(BeEmpty())
	})

	It("deletes its states when the policy is deleted", func() {
		Expect(k8sClient.Create(ctx, newPolicy(policyA))).To(Succeed())
		reconcileVTP(policyA)
		Expect(getState().Status.PolicyRef).To(Equal(policyA))

		By("deleting the policy; the finalizer removes its states")
		Expect(k8sClient.Delete(ctx, getPolicy(policyA))).To(Succeed())
		reconcileVTP(policyA)

		Expect(stateGone()).To(BeTrue())
		Expect(errors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: namespace, Name: policyA}, &zanderykgithubiov1.VolumeTierPolicy{}))).To(BeTrue())
	})

	Describe("tier validation", func() {
		// expectNotReady reconciles the policy and expects Ready False with
		// reason, a message containing msg, and no state for the PVC.
		expectNotReady := func(vtp *zanderykgithubiov1.VolumeTierPolicy, reason VTPConditionReason, msg string) {
			Expect(k8sClient.Create(ctx, vtp)).To(Succeed())
			reconcileVTP(vtp.Name)

			ready := meta.FindStatusCondition(getPolicy(vtp.Name).Status.Conditions, string(ConditionVTPReady))
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(string(reason)))
			Expect(ready.Message).To(ContainSubstring(msg))
			Expect(errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pvcName},
				&zanderykgithubiov1.VolumeTierState{}))).To(BeTrue(), "state created for an invalid policy")
		}

		It("reports a missing VolumeAttributesClass", func() {
			vtp := newPolicy(policyA)
			vtp.Spec.Tiers = append(vtp.Spec.Tiers, zanderykgithubiov1.Tier{VACName: "test-tier-missing"})
			expectNotReady(vtp, VTPConditionReasonVolumeAttributesClassNotFound, "test-tier-missing")
		})

		It("reports tiers of different drivers", func() {
			newOtherPolicy(policyB) // creates the other driver's classes
			vtp := newPolicy(policyA)
			vtp.Spec.Tiers = []zanderykgithubiov1.Tier{{VACName: vacLow}, {VACName: otherHigh}}
			// Sorted by driver.
			expectNotReady(vtp, VTPConditionReasonDriverMismatch,
				otherDriver+" ("+otherHigh+"); "+driver+" ("+vacLow+")")
		})

		It("reports ceilings that do not increase", func() {
			vtp := newPolicy(policyA)
			vtp.Spec.Tiers = []zanderykgithubiov1.Tier{
				{VACName: vacLow, Ceilings: &zanderykgithubiov1.Ceiling{IOPS: ptr.To[int64](5000)}},
				{VACName: vacHigh, Ceilings: &zanderykgithubiov1.Ceiling{IOPS: ptr.To[int64](3000)}},
			}
			expectNotReady(vtp, VTPConditionReasonInvalidTiers, "iops ceiling 3000 must be greater")
		})

		It("becomes Ready once the missing class is created", func() {
			const missing = "test-tier-late"
			vtp := newPolicy(policyA)
			vtp.Spec.Tiers = append(vtp.Spec.Tiers, zanderykgithubiov1.Tier{VACName: missing})
			expectNotReady(vtp, VTPConditionReasonVolumeAttributesClassNotFound, missing)

			vac := &storagev1.VolumeAttributesClass{Name: missing, DriverName: driver, Parameters: map[string]string{vacParam: missing}}
			Expect(k8sClient.Create(ctx, vac)).To(Succeed())
			DeferCleanup(forceDelete, vac)
			reconcileVTP(policyA)

			Expect(meta.IsStatusConditionTrue(getPolicy(policyA).Status.Conditions, string(ConditionVTPReady))).To(BeTrue())
		})
	})

	Describe("watch mappings", func() {
		It("maps a PVC or policy event to every policy in the same namespace", func() {
			Expect(k8sClient.Create(ctx, newPolicy(policyA))).To(Succeed())
			Expect(k8sClient.Create(ctx, newPolicy(policyB))).To(Succeed())
			r := &VolumeTierPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			want := []reconcile.Request{
				{Namespace: namespace, Name: policyA},
				{Namespace: namespace, Name: policyB},
			}

			pvc := &corev1.PersistentVolumeClaim{Name: pvcName, Namespace: namespace}
			Expect(r.policiesInNamespace(ctx, pvc)).To(ConsistOf(want))
			Expect(r.policiesInNamespace(ctx, getPolicy(policyA))).To(ConsistOf(want))

			elsewhere := &corev1.PersistentVolumeClaim{Name: pvcName, Namespace: "other"}
			Expect(r.policiesInNamespace(ctx, elsewhere)).To(BeEmpty())
		})

		It("maps a policy to the states it manages", func() {
			Expect(k8sClient.Create(ctx, newPolicy(policyA))).To(Succeed())
			reconcileVTP(policyA)
			r := &VolumeTierStateReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			Expect(r.statesForPolicy(ctx, getPolicy(policyA))).To(ConsistOf(
				reconcile.Request{Namespace: namespace, Name: pvcName}))

			other := &zanderykgithubiov1.VolumeTierPolicy{Name: policyB, Namespace: namespace}
			Expect(r.statesForPolicy(ctx, other)).To(BeEmpty())
		})
	})
})
