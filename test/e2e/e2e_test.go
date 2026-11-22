//go:build e2e

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

package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// namespace where the project is deployed in
const namespace = "tierd-system"

// serviceAccountName created for the project
const serviceAccountName = "tierd-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "tierd-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "tierd-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	curlPod := &corev1.Pod{Namespace: namespace, Name: "curl-metrics"}
	binding := &rbacv1.ClusterRoleBinding{
		Name:    metricsRoleBindingName,
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "tierd-metrics-reader"},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Namespace: namespace, Name: serviceAccountName,
		}},
	}

	// tierd is deployed once for every spec in BeforeSuite (e2e_suite_test.go).
	AfterAll(func() {
		By("cleaning up the curl pod and metrics binding")
		remove(curlPod, binding)
	})

	// After each test, check for failures and collect logs and events for
	// debugging.
	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		dumpDiagnostics(namespace)
		if out, err := podLogs(curlPod.Name, 0); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n%s\n", out)
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			Eventually(func(g Gomega) {
				pods := &corev1.PodList{}
				g.Expect(k8s.List(ctx, pods, client.InNamespace(namespace),
					client.MatchingLabels{"control-plane": "controller-manager"})).To(Succeed())
				var running []corev1.Pod
				for _, p := range pods.Items {
					if p.DeletionTimestamp == nil {
						running = append(running, p)
					}
				}
				g.Expect(running).To(HaveLen(1), "expected 1 controller pod running")
				g.Expect(running[0].Status.Phase).To(Equal(corev1.PodRunning), "Incorrect controller-manager pod status")
				controllerPodName = running[0].Name
			}).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			if err := k8s.Create(ctx, binding); !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")
			}

			By("validating that the metrics service is available")
			Expect(k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: metricsServiceName}, &corev1.Service{})).
				To(Succeed(), "Metrics service should exist")

			By("getting the service account token")
			tr, err := clientset.CoreV1().ServiceAccounts(namespace).
				CreateToken(ctx, serviceAccountName, &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			token := tr.Status.Token
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			Eventually(func() bool { return podReady(namespace, controllerPodName) }, 3*time.Minute).
				Should(BeTrue(), "Controller pod not ready")

			By("verifying that the controller manager is serving the metrics server")
			Eventually(func() (string, error) { return podLogs(controllerPodName, 0) }, 3*time.Minute).
				Should(ContainSubstring("Serving metrics server"), "Metrics server not yet started")

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			pod := curlPod.DeepCopy()
			pod.Spec = corev1.PodSpec{
				RestartPolicy:      corev1.RestartPolicyNever,
				ServiceAccountName: serviceAccountName,
				Containers: []corev1.Container{{
					Name:    "curl",
					Image:   "curlimages/curl:latest",
					Command: []string{"/bin/sh", "-c"},
					Args: []string{fmt.Sprintf(
						"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' "+
							"https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1",
						token, metricsServiceName, namespace)},
					SecurityContext: &corev1.SecurityContext{
						ReadOnlyRootFilesystem:   new(true),
						AllowPrivilegeEscalation: new(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						RunAsNonRoot:             new(true),
						RunAsUser:                new(int64(1000)),
						SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
				}},
			}
			Expect(k8s.Create(ctx, pod)).To(Succeed(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			Eventually(func() corev1.PodPhase { return fetch(namespace, pod.Name, &corev1.Pod{}).Status.Phase },
				5*time.Minute).Should(Equal(corev1.PodSucceeded), "curl pod in wrong status")

			By("getting the metrics by checking curl-metrics logs")
			Eventually(func() (string, error) { return podLogs(pod.Name, 0) }).
				Should(ContainSubstring("< HTTP/1.1 200 OK"))
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})
})
