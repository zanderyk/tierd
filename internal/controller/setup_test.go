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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var _ = Describe("SetupWithManager", func() {
	It("registers both controllers and their indexes with a manager", func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
			// Other specs may build managers with the same controller names.
			Controller: config.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect((&VolumeTierPolicyReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).
			SetupWithManager(mgr)).To(Succeed())
		Expect((&VolumeTierStateReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).
			SetupWithManager(mgr)).To(Succeed())

		By("rejecting a second registration of the same index")
		Expect((&VolumeTierStateReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).
			SetupWithManager(mgr)).To(MatchError(ContainSubstring("cannot index VolumeTierState policyRef")))
		Expect((&VolumeTierPolicyReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).
			SetupWithManager(mgr)).To(MatchError(ContainSubstring("cannot index VolumeTierPolicy tiers")))
	})
})
