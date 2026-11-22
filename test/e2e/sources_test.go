//go:build e2e
// +build e2e

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

package e2e

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/zanderyk/tierd/test/utils"
)

const (
	sourcesNS = "tierd-sources"

	nfsDriverURL = "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/v4.13.6/deploy/v4.13.6/"
	smbDriverURL = "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-smb/v1.20.4/deploy/v1.20.4/"
)

// sourcesSetup is an NFS server and an SMB server in the cluster, their
// StorageClasses, and VolumeAttributesClasses for the two drivers. Neither
// driver can modify volumes, so their policies only recommend; the PVCs have
// no class and are adopted. The servers keep their data on the node's own
// disk, not the loop device the hostpath volumes share.
const sourcesSetup = `
apiVersion: v1
kind: Namespace
metadata:
  name: tierd-sources
---
apiVersion: v1
kind: Service
metadata:
  name: nfs-server
  namespace: tierd-sources
spec:
  selector:
    app: nfs-server
  ports:
  - {name: nfs, port: 2049}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nfs-server
  namespace: tierd-sources
spec:
  selector:
    matchLabels:
      app: nfs-server
  template:
    metadata:
      labels:
        app: nfs-server
    spec:
      containers:
      - name: nfs-server
        image: itsthenetwork/nfs-server-alpine:12
        env:
        - {name: SHARED_DIRECTORY, value: /exports}
        securityContext:
          privileged: true
        volumeMounts:
        - {name: data, mountPath: /exports}
      volumes:
      - name: data
        hostPath: {path: /var/lib/tierd-nfs, type: DirectoryOrCreate}
---
apiVersion: v1
kind: Secret
metadata:
  name: smbcreds
  namespace: tierd-sources
stringData:
  username: tierd
  password: tierd-e2e
---
apiVersion: v1
kind: Service
metadata:
  name: smb-server
  namespace: tierd-sources
spec:
  selector:
    app: smb-server
  ports:
  - {name: smb, port: 445}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: smb-server
  namespace: tierd-sources
spec:
  selector:
    matchLabels:
      app: smb-server
  template:
    metadata:
      labels:
        app: smb-server
    spec:
      containers:
      - name: smb-server
        image: andyzhangx/samba:win-fix
        env:
        - {name: PERMISSIONS, value: "0777"}
        - name: USERNAME
          valueFrom: {secretKeyRef: {name: smbcreds, key: username}}
        - name: PASSWORD
          valueFrom: {secretKeyRef: {name: smbcreds, key: password}}
        args: ["-u", "$(USERNAME);$(PASSWORD)", "-s", "share;/smbshare/;yes;no;no;all;none", "-p"]
        volumeMounts:
        - {name: data, mountPath: /smbshare}
      volumes:
      - name: data
        hostPath: {path: /var/lib/tierd-smb, type: DirectoryOrCreate}
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: e2e-nfs
provisioner: nfs.csi.k8s.io
parameters:
  server: nfs-server.tierd-sources.svc.cluster.local
  share: /
mountOptions: [nfsvers=4.1]
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: e2e-smb
provisioner: smb.csi.k8s.io
parameters:
  source: //smb-server.tierd-sources.svc.cluster.local/share
  csi.storage.k8s.io/provisioner-secret-name: smbcreds
  csi.storage.k8s.io/provisioner-secret-namespace: tierd-sources
  csi.storage.k8s.io/node-stage-secret-name: smbcreds
  csi.storage.k8s.io/node-stage-secret-namespace: tierd-sources
mountOptions: [dir_mode=0777, file_mode=0777, noperm, mfsymlinks, cache=strict, noserverino]
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: e2e-nfs-base
driverName: nfs.csi.k8s.io
parameters:
  tier: base
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: e2e-nfs-fast
driverName: nfs.csi.k8s.io
parameters:
  tier: fast
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: e2e-smb-base
driverName: smb.csi.k8s.io
parameters:
  tier: base
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: e2e-smb-fast
driverName: smb.csi.k8s.io
parameters:
  tier: fast
`

var _ = Describe("Measurement sources", Ordered, func() {
	SetDefaultEventuallyTimeout(4 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	BeforeAll(func() {
		By("installing the NFS and SMB CSI drivers")
		args := []string{"apply"}
		for _, f := range []string{"rbac-csi-nfs.yaml", "csi-nfs-driverinfo.yaml", "csi-nfs-controller.yaml", "csi-nfs-node.yaml"} {
			args = append(args, "-f", nfsDriverURL+f)
		}
		for _, f := range []string{"rbac-csi-smb.yaml", "csi-smb-driver.yaml", "csi-smb-controller.yaml", "csi-smb-node.yaml"} {
			args = append(args, "-f", smbDriverURL+f)
		}
		kubectl(args...)

		By("creating the servers, StorageClasses and tiers")
		apply(hostpathTiers + "---" + sourcesSetup)

		By("creating a PVC, policy and fio pod per source")
		apply(policyIn(sourcesNS, "block", "e2e-block", "Act", tierBase, tierFast) + "---" +
			pvcIn(sourcesNS, "block", "e2e-block", "e2e-hostpath", tierBase, true) + "---" +
			loadPodIn(sourcesNS, "block", "block", "sustained", true, true) + "---" +
			policyIn(sourcesNS, "nfs", "e2e-nfs", "Recommend", "e2e-nfs-base", "e2e-nfs-fast") + "---" +
			pvcIn(sourcesNS, "nfs", "e2e-nfs", "e2e-nfs", "", false) + "---" +
			loadPodIn(sourcesNS, "nfs", "nfs", "sustained", false, true) + "---" +
			policyIn(sourcesNS, "smb", "e2e-smb", "Recommend", "e2e-smb-base", "e2e-smb-fast") + "---" +
			pvcIn(sourcesNS, "smb", "e2e-smb", "e2e-smb", "", false) + "---" +
			// cifs needs cache=none for O_DIRECT; fsync each write instead.
			loadPodIn(sourcesNS, "smb", "smb", "sustained", false, false))

		for _, w := range []string{"deployment/csi-nfs-controller", "daemonset/csi-nfs-node",
			"deployment/csi-smb-controller", "daemonset/csi-smb-node"} {
			kubectl("rollout", "status", w, "-n", "kube-system", "--timeout=5m")
		}
		kubectl("wait", "pod", "--all", "-n", sourcesNS, "--for=condition=Ready", "--timeout=5m")
	})

	AfterAll(func() {
		// Delete the fio pods first: deleting the namespace would take the
		// NFS and SMB servers down before the pods unmount, and the
		// unmounts hang.
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", "block", "nfs", "smb", "-n", sourcesNS, "--timeout=2m"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", sourcesNS, "--timeout=2m"))
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		for _, args := range [][]string{
			{"get", "vtp,vts,volumeiosample,pvc,pod", "-n", sourcesNS, "-o", "yaml"},
			{"get", "events", "-n", sourcesNS, "--sort-by=.lastTimestamp"},
			{"logs", "daemonset/" + agentDaemonSet, "-n", namespace, "--tail=100"},
		} {
			out, _ := utils.Run(exec.Command("kubectl", args...))
			_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n", strings.Join(args, " "), out)
		}
	})

	It("measures and tiers a block-mode volume", func() {
		expectMeasured("block", "Complete")
		Eventually(func() string { return getIn(sourcesNS, "pvc", "block", "{.status.currentVolumeAttributesClassName}") }).
			Should(Equal(tierFast))
	})

	It("measures an NFS volume, with latency", func() {
		expectMeasured("nfs", "Partial")
		Expect(getIn(sourcesNS, "volumeiosample", "nfs", "{.status.latencyP95Micros}")).NotTo(BeEmpty())
		Eventually(func() string { return getIn(sourcesNS, "vts", "nfs", "{.status.recommendedTier}") }).
			Should(Equal("e2e-nfs-fast"))
	})

	It("measures an SMB volume on a subdirectory of its share", func() {
		expectMeasured("smb", "Partial")
		Eventually(func() string { return getIn(sourcesNS, "vts", "smb", "{.status.recommendedTier}") }).
			Should(Equal("e2e-smb-fast"))
	})
})

// expectMeasured waits for the PVC's sample to carry the Measured reason and
// show the fio pod's writes, and for its state to be managed.
func expectMeasured(pvc, reason string) {
	GinkgoHelper()
	Eventually(func() string {
		return getIn(sourcesNS, "volumeiosample", pvc, `{.status.conditions[?(@.type=="Measured")].reason}`)
	}).Should(Equal(reason))
	Eventually(func() int {
		n, _ := strconv.Atoi(getIn(sourcesNS, "volumeiosample", pvc, "{.status.iops}"))
		return n
	}).Should(BeNumerically(">", 100))
	Eventually(func() string {
		return getIn(sourcesNS, "vts", pvc, `{.status.conditions[?(@.type=="Managed")].status}`)
	}).Should(Equal("True"))
}
