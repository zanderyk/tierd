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
	// controllers, so protection finalizers (pvc-protection, vac-protection)
	// are stripped by hand.
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
		By("creating the tier VolumeAttributesClasses and a labelled PVC")
		for _, name := range []string{vacLow, vacHigh} {
			Expect(k8sClient.Create(ctx, &storagev1.VolumeAttributesClass{
				Name:       name,
				DriverName: driver,
				Parameters: map[string]string{"tier": name},
			})).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &corev1.PersistentVolumeClaim{
			Name:      pvcName,
			Namespace: namespace,
			Labels:    pvcLabels,
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		By("cleaning up policies, state, PVC and VolumeAttributesClasses")
		Expect(k8sClient.DeleteAllOf(ctx, &zanderykgithubiov1.VolumeTierPolicy{}, client.InNamespace(namespace))).To(Succeed())
		deleteIgnoreNotFound(&zanderykgithubiov1.VolumeTierState{Name: pvcName, Namespace: namespace})

		forceDelete(&corev1.PersistentVolumeClaim{Name: pvcName, Namespace: namespace})
		for _, name := range []string{vacLow, vacHigh} {
			forceDelete(&storagev1.VolumeAttributesClass{Name: name})
		}
	})

	It("assigns the PVC to the only matching policy", func() {
		Expect(k8sClient.Create(ctx, newPolicy("policy-a"))).To(Succeed())

		res := reconcileVTP("policy-a")
		Expect(res.RequeueAfter).To(Equal(membershipResyncInterval))

		vtp := getPolicy("policy-a")
		Expect(meta.IsStatusConditionTrue(vtp.Status.Conditions, string(ConditionVTPReady))).To(BeTrue())
		Expect(meta.IsStatusConditionFalse(vtp.Status.Conditions, string(ConditionVTPConflict))).To(BeTrue())
		Expect(vtp.Status.PVCs).To(Equal([]string{pvcName}))

		state := getState()
		Expect(state.Status.PolicyRef).To(Equal("policy-a"))
		Expect(state.OwnerReferences).To(HaveLen(1))
		Expect(state.OwnerReferences[0].Name).To(Equal(pvcName))
	})

	It("leaves the PVC unmanaged and reports Conflict when two policies match it", func() {
		Expect(k8sClient.Create(ctx, newPolicy("policy-a"))).To(Succeed())
		reconcileVTP("policy-a")
		Expect(getState().Status.PolicyRef).To(Equal("policy-a"))

		By("adding a second policy with an overlapping selector")
		Expect(k8sClient.Create(ctx, newPolicy("policy-b"))).To(Succeed())
		reconcileVTP("policy-b")
		reconcileVTP("policy-a")

		Expect(getState().Status.PolicyRef).To(BeEmpty())
		for _, name := range []string{"policy-a", "policy-b"} {
			cond := meta.FindStatusCondition(getPolicy(name).Status.Conditions, string(ConditionVTPConflict))
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(string(VTPConditionReasonPolicyConflict)))
			Expect(cond.Message).To(ContainSubstring("%s (policy-a, policy-b)", pvcName))
		}

		By("deleting one policy; the survivor claims the PVC on its next reconcile")
		Expect(k8sClient.Delete(ctx, getPolicy("policy-b"))).To(Succeed())
		reconcileVTP("policy-a")

		Expect(getState().Status.PolicyRef).To(Equal("policy-a"))
		Expect(meta.IsStatusConditionFalse(getPolicy("policy-a").Status.Conditions, string(ConditionVTPConflict))).To(BeTrue())
	})
})
