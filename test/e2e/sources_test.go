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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
	"github.com/zanderyk/tierd/internal/controller"
)

const (
	sourcesNS = "tierd-sources"

	nfsDriverURL = "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/v4.13.6/deploy/v4.13.6/"
	smbDriverURL = "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-smb/v1.20.4/deploy/v1.20.4/"
)

// source is one kind of volume the agent measures: a PVC with a policy over
// its driver's tiers, and a fio pod writing to it.
type source struct {
	name       string
	sc         string
	mode       tierdv1.VolumeTierPolicyMode
	base, fast string
	// tier is the PVC's starting tier; empty means tierd adopts it.
	tier          string
	block, direct bool
}

// sources: the hostpath block volume can be tiered. The NFS and SMB drivers
// can't modify volumes, so their policies only recommend, and their PVCs
// start without a class. cifs needs cache=none for O_DIRECT, so the SMB pod
// fsyncs each write instead.
var sources = []source{
	{name: "block", sc: "e2e-hostpath", mode: tierdv1.VolumeTierPolicyModeAct, base: tierBase, fast: tierFast,
		tier: tierBase, block: true, direct: true},
	{name: "nfs", sc: "e2e-nfs", mode: tierdv1.VolumeTierPolicyModeRecommend, base: "e2e-nfs-base",
		fast: "e2e-nfs-fast", direct: true},
	{name: "smb", sc: "e2e-smb", mode: tierdv1.VolumeTierPolicyModeRecommend, base: "e2e-smb-base",
		fast: "e2e-smb-fast"},
}

var _ = Describe("Measurement sources", Ordered, func() {
	SetDefaultEventuallyTimeout(4 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	BeforeAll(func() {
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

		By("creating a PVC, policy and fio pod per source")
		for _, s := range sources {
			app := "e2e-" + s.name
			create(newPolicy(sourcesNS, s.name, app, s.mode, s.base, s.fast),
				newPVC(sourcesNS, s.name, app, s.sc, s.tier, s.block),
				newLoadPod(sourcesNS, s.name, s.name, "sustained", s.block, s.direct))
		}

		for _, name := range []string{"csi-nfs-controller", "csi-smb-controller"} {
			waitRolledOut(&appsv1.Deployment{Namespace: "kube-system", Name: name})
		}
		for _, name := range []string{"csi-nfs-node", "csi-smb-node"} {
			waitRolledOut(&appsv1.DaemonSet{Namespace: "kube-system", Name: name})
		}
		for _, s := range sources {
			Eventually(func() bool { return podReady(sourcesNS, s.name) }, 5*time.Minute).
				Should(BeTrue(), "fio pod %s did not become ready", s.name)
		}
	})

	AfterAll(func() {
		// Delete the fio pods first: deleting the namespace would take the
		// NFS and SMB servers down before the pods unmount, and the
		// unmounts hang.
		for _, s := range sources {
			removeAndWait(&corev1.Pod{Namespace: sourcesNS, Name: s.name})
		}
		removeAndWait(&corev1.Namespace{Name: sourcesNS})
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics(sourcesNS)
		}
	})

	It("measures and tiers a block-mode volume", func() {
		expectMeasured("block", tierdv1.MeasuredComplete)
		Eventually(func() string { return currentTier(sourcesNS, "block") }).Should(Equal(tierFast))
	})

	It("measures an NFS volume, with latency", func() {
		expectMeasured("nfs", tierdv1.MeasuredPartial)
		Expect(getSample(sourcesNS, "nfs").Status.LatencyP95Micros).NotTo(BeNil())
		Eventually(func() string { return getVTS(sourcesNS, "nfs").Status.RecommendedTier }).
			Should(Equal("e2e-nfs-fast"))
	})

	It("measures an SMB volume on a subdirectory of its share", func() {
		expectMeasured("smb", tierdv1.MeasuredPartial)
		Eventually(func() string { return getVTS(sourcesNS, "smb").Status.RecommendedTier }).
			Should(Equal("e2e-smb-fast"))
	})
})

// expectMeasured waits for the PVC's sample to carry the Measured reason and
// show the fio pod's writes, and for its state to be managed.
func expectMeasured(pvc, reason string) {
	GinkgoHelper()
	Eventually(func() string {
		return condition(getSample(sourcesNS, pvc).Status.Conditions, tierdv1.VolumeIOSampleMeasured).Reason
	}).Should(Equal(reason))
	Eventually(func() int64 { return getSample(sourcesNS, pvc).Status.IOPS }).Should(BeNumerically(">", 100))
	Eventually(func() metav1.ConditionStatus {
		return condition(getVTS(sourcesNS, pvc).Status.Conditions, controller.ConditionVTSManaged).Status
	}).Should(Equal(metav1.ConditionTrue))
}
