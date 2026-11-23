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
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"

	"github.com/zanderyk/tierd/test/utils"
)

const (
	agentDaemonSet = "tierd-agent"
	// hostpathDataDir is where the CSI hostpath driver keeps volumes on the
	// node. The suite mounts it on a loop device so every e2e volume sits on
	// one real block device the agent can measure.
	hostpathDataDir = "/var/lib/csi-hostpath-data"
)

// TestE2E runs the e2e suite against the cluster in $KUBECONFIG, set up as
// the profile in $E2E_PROFILE says (profile_test.go): by default the kind
// cluster `make test-e2e` creates.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting tierd e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	setupClient()
	_, _ = fmt.Fprintf(GinkgoWriter, "Running against %s with profile %+v\n", restCfg.Host, *profile)

	if profile.Kind {
		By("building the manager image")
		_, err := utils.Run(exec.Command("make", "docker-build", "IMG="+profile.Image))
		Expect(err).NotTo(HaveOccurred(), "Failed to build the manager image")

		By("loading the manager image on Kind")
		Expect(utils.LoadImageToKindClusterWithName(profile.Image)).To(Succeed(),
			"Failed to load the manager image into Kind")

		setupHostpathDriver()
	}
	if profile.Deploy {
		deployTierd()
	}

	By("creating the profile's StorageClasses and VolumeAttributesClasses")
	for _, m := range profile.Manifests {
		applyFile(m)
	}
})

// setupHostpathDriver mounts the hostpath data dir on a loop device in the
// kind node, then deploys the CSI hostpath driver with ModifyVolume enabled.
func setupHostpathDriver() {
	By("mounting the hostpath data dir on a loop device")
	// mount -o loop sets autoclear, so the loop device is released when the
	// kind node goes away.
	script := fmt.Sprintf(`set -e
mountpoint -q %[1]s && exit 0
truncate -s 2G /var/lib/tierd-e2e.img
mkfs.ext4 -q -F /var/lib/tierd-e2e.img
mkdir -p %[1]s
mount -o loop /var/lib/tierd-e2e.img %[1]s`, hostpathDataDir)
	_, err := utils.Run(exec.Command("docker", "exec", utils.KindNode(), "sh", "-c", script))
	Expect(err).NotTo(HaveOccurred(), "Failed to mount the hostpath data dir")

	By("deploying the CSI hostpath driver")
	applyFile("test/e2e/testdata/csi-hostpath.yaml")
	waitRolledOut(&appsv1.StatefulSet{Namespace: "csi-hostpath", Name: "csi-hostpathplugin"})
}

// deployTierd installs the CRDs and deploys the manager and agent with
// kustomize, then sets the agent's intervals from the profile.
func deployTierd() {
	By("installing CRDs")
	_, err := utils.Run(exec.Command("make", "install"))
	Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

	By("deploying the controller-manager and agent")
	_, err = utils.Run(exec.Command("make", "deploy", "IMG="+profile.Image))
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy tierd")

	By("setting the agent's intervals")
	a := profile.Agent
	agent := &appsv1.DaemonSet{Namespace: namespace, Name: agentDaemonSet}
	update(agent, func(ds *appsv1.DaemonSet) {
		c := &ds.Spec.Template.Spec.Containers[0]
		c.Args = append(c.Args, "--sample-interval="+a.SampleInterval.Duration.String(),
			"--publish-interval="+a.PublishInterval.Duration.String(), "--window="+a.Window.Duration.String())
	})

	waitRolledOut(agent)
	waitRolledOut(&appsv1.Deployment{Namespace: namespace, Name: "tierd-controller-manager"})
}

func init() {
	// kubectl reads the user's kuberc unless told not to; make runs it.
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		_ = os.Setenv("KUBECTL_KUBERC", "false")
	}
}
