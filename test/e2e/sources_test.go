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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	tierdv1alpha1 "github.com/zanderyk/tierd/api/v1alpha1"
	"github.com/zanderyk/tierd/internal/controller"
)

const (
	sourcesNS = "tierd-sources"

	nfsDriverURL = "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/v4.13.6/deploy/v4.13.6/"
	smbDriverURL = "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-smb/v1.20.4/deploy/v1.20.4/"
)

var _ = Describe("Measurement sources", Ordered, Label("sources"), func() {
	SetDefaultEventuallyTimeout(profile.Timeout.Duration)
	SetDefaultEventuallyPollingInterval(time.Second)

	BeforeAll(func() {
		if profile.Kind {
			installKindSources()
		} else {
			createIfMissing(&corev1.Namespace{Name: sourcesNS})
		}

		By("creating a PVC, policy and fio pod per source")
		for _, s := range profile.Sources {
			app := "e2e-" + s.Name
			tier := ""
			if s.StartOnBase {
				tier = s.Base.Name
			}
			create(newPolicy(sourcesNS, s.Name, app, s.Mode, s.Base, s.Fast),
				newPVC(sourcesNS, s.Name, app, s.StorageClass, tier, s.Block),
				newLoadPod(sourcesNS, s.Name, s.Name, "sustained", s.Block, s.Direct))
		}
		for _, s := range profile.Sources {
			Eventually(func() bool { return podReady(sourcesNS, s.Name) }, profile.Timeout.Duration).
				Should(BeTrue(), "fio pod %s did not become ready", s.Name)
		}
	})

	AfterAll(func() {
		// Delete the fio pods first: on kind, deleting the namespace would
		// take the NFS and SMB servers down before the pods unmount, and the
		// unmounts hang.
		for _, s := range profile.Sources {
			removeAndWait(&corev1.Pod{Namespace: sourcesNS, Name: s.Name})
		}
		removeAndWait(&corev1.Namespace{Name: sourcesNS})
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics(sourcesNS)
		}
	})

	for _, s := range profile.Sources {
		It("measures the "+s.Name+" volume", func() {
			Eventually(func() string {
				return condition(getSample(sourcesNS, s.Name).Status.Conditions, tierdv1alpha1.VolumeIOSampleMeasured).Reason
			}).Should(Equal(s.Measured))
			Eventually(func() int64 { return getSample(sourcesNS, s.Name).Status.IOPS }).Should(BeNumerically(">", 0))
			Eventually(func() metav1.ConditionStatus {
				return condition(getVTS(sourcesNS, s.Name).Status.Conditions, controller.ConditionVTSManaged).Status
			}).Should(Equal(metav1.ConditionTrue))
			if s.Latency {
				Expect(getSample(sourcesNS, s.Name).Status.LatencyP95Micros).NotTo(BeNil(), "latency")
			}

			if s.Mode == tierdv1alpha1.VolumeTierPolicyModeAct {
				By("waiting for the PVC to move to " + s.Fast.Name)
				Eventually(func() string { return currentTier(sourcesNS, s.Name) }).Should(Equal(s.Fast.Name))
			} else {
				By("waiting for tierd to recommend " + s.Fast.Name)
				Eventually(func() string { return getVTS(sourcesNS, s.Name).Status.RecommendedTier }).
					Should(Equal(s.Fast.Name), func() string {
						st := getSample(sourcesNS, s.Name).Status
						return fmt.Sprintf("sample: %d IOPS, needs > %d (80%% of %s)",
							st.IOPS, s.Base.IOPS*8/10, s.Base.Name)
					})
			}
		})
	}
})

// installKindSources runs NFS and SMB servers in the kind cluster and
// installs their CSI drivers. The servers' namespace is the sources
// namespace.
func installKindSources() {
	GinkgoHelper()
	By("installing the NFS and SMB CSI drivers")
	for _, f := range []string{"rbac-csi-nfs.yaml", "csi-nfs-driverinfo.yaml",
		"csi-nfs-controller.yaml", "csi-nfs-node.yaml"} {
		applyURL(nfsDriverURL + f)
	}
	for _, f := range []string{"rbac-csi-smb.yaml", "csi-smb-driver.yaml",
		"csi-smb-controller.yaml", "csi-smb-node.yaml"} {
		applyURL(smbDriverURL + f)
	}

	By("creating the NFS and SMB servers")
	applyFile("test/e2e/testdata/servers.yaml")

	for _, name := range []string{"csi-nfs-controller", "csi-smb-controller"} {
		waitRolledOut(&appsv1.Deployment{Namespace: "kube-system", Name: name})
	}
	for _, name := range []string{"csi-nfs-node", "csi-smb-node"} {
		waitRolledOut(&appsv1.DaemonSet{Namespace: "kube-system", Name: name})
	}
}
