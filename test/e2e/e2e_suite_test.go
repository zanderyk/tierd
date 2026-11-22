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
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/zanderyk/tierd/test/utils"
)

var (
	// managerImage is the manager and agent image built and loaded into kind.
	managerImage = "tierd.local/tierd:e2e"
	// shouldCleanupCertManager tracks whether CertManager was installed by this suite.
	shouldCleanupCertManager = false
)

const (
	agentDaemonSet = "tierd-agent"
	// hostpathDataDir is where the CSI hostpath driver keeps volumes on the
	// node. The suite mounts it on a loop device so every e2e volume sits on
	// one real block device the agent can measure.
	hostpathDataDir = "/var/lib/csi-hostpath-data"
)

// TestE2E runs the e2e suite against the kind cluster that `make test-e2e`
// creates. It deploys the CSI hostpath driver and tierd once, then runs the
// specs.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting tierd e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	configureKubectlKubeRC()
	setupCertManager()
	setupHostpathDriver()
	deployTierd()
})

var _ = AfterSuite(func() {
	teardownCertManager()
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
	cmd := exec.Command("docker", "exec", utils.KindNode(), "sh", "-c", script)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to mount the hostpath data dir")

	By("deploying the CSI hostpath driver")
	cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/testdata/csi-hostpath.yaml")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy the CSI hostpath driver")
	cmd = exec.Command("kubectl", "rollout", "status", "statefulset/csi-hostpathplugin",
		"-n", "csi-hostpath", "--timeout=5m")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "CSI hostpath driver did not become ready")
}

// deployTierd installs the CRDs and deploys the manager and agent. The agent
// samples every second and publishes every second over a 3s window so the
// specs' `for` windows can be short.
func deployTierd() {
	By("installing CRDs")
	cmd := exec.Command("make", "install")
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

	By("deploying the controller-manager and agent")
	cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to deploy tierd")

	By("speeding up the agent")
	cmd = exec.Command("kubectl", "patch", "daemonset", agentDaemonSet, "-n", namespace, "--type=json", "-p",
		`[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--sample-interval=1s"},
		  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--publish-interval=1s"},
		  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--window=3s"}]`)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to patch the agent")

	for _, workload := range []string{"daemonset/" + agentDaemonSet, "deployment/tierd-controller-manager"} {
		cmd = exec.Command("kubectl", "rollout", "status", workload, "-n", namespace, "--timeout=5m")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "%s did not become ready", workload)
	}
}

func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		By("disabling kubectl kuberc for test isolation")
		err := os.Setenv("KUBECTL_KUBERC", "false")
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to disable kubectl kuberc")
		_, _ = fmt.Fprintf(GinkgoWriter,
			"kubectl kuberc disabled for consistent test behavior (override with KUBECTL_KUBERC=true)\n")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")
	}
}

// setupCertManager installs CertManager if needed for webhook tests.
// Skips installation if CERT_MANAGER_INSTALL_SKIP=true or if already present.
func setupCertManager() {
	if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager installation (CERT_MANAGER_INSTALL_SKIP=true)\n")
		return
	}

	By("checking if CertManager is already installed")
	if utils.IsCertManagerCRDsInstalled() {
		_, _ = fmt.Fprintf(GinkgoWriter, "CertManager is already installed. Skipping installation.\n")
		return
	}

	// Mark for cleanup before installation to handle interruptions and partial installs.
	shouldCleanupCertManager = true

	By("installing CertManager")
	Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
}

// teardownCertManager uninstalls CertManager if it was installed by setupCertManager.
// This ensures we only remove what we installed.
func teardownCertManager() {
	if !shouldCleanupCertManager {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping CertManager cleanup (not installed by this suite)\n")
		return
	}

	By("uninstalling CertManager")
	utils.UninstallCertManager()
}
