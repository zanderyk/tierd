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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/zanderyk/tierd/test/utils"
)

const (
	tieringNS = "tierd-e2e"
	tierBase  = "e2e-base"
	tierFast  = "e2e-fast"
	loadPVC   = "load"
	loadPod   = "load"

	// scaleFor is the policy's scale-up and scale-down `for` window.
	scaleFor = 10 * time.Second
)

// hostpathTiers is the hostpath StorageClass and the two tiers. The hostpath
// driver has no real limits, so the policies' ceilings are made up.
const hostpathTiers = `
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: e2e-hostpath
provisioner: hostpath.csi.k8s.io
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: e2e-base
driverName: hostpath.csi.k8s.io
parameters:
  tier: base
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: e2e-fast
driverName: hostpath.csi.k8s.io
parameters:
  tier: fast
`

// tieringSetup is the namespace and everything the specs share: the hostpath
// tiers, a VAC the driver rejects, and a StorageClass of a driver that isn't
// installed.
const tieringSetup = `
apiVersion: v1
kind: Namespace
metadata:
  name: tierd-e2e
---
` + hostpathTiers + `
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: e2e-rejected
driverName: hostpath.csi.k8s.io
parameters:
  iops: "1000"
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: e2e-other
provisioner: other.csi.example.com
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: e2e-other-base
driverName: other.csi.example.com
parameters:
  tier: base
---
apiVersion: storage.k8s.io/v1
kind: VolumeAttributesClass
metadata:
  name: e2e-other-fast
driverName: other.csi.example.com
parameters:
  tier: fast
`

// policy returns an Act-mode policy in tierd-e2e over the hostpath tiers
// selecting app=<app>.
func policy(name, app string) string {
	return policyIn(tieringNS, name, app, "Act", tierBase, tierFast)
}

// policyIn returns a policy over tiers base and fast. base tops out at 100
// IOPS and fast at 100k; throughput ceilings are high so IOPS alone decides.
func policyIn(ns, name, app, mode, base, fast string) string {
	return fmt.Sprintf(`
apiVersion: tierd.zanderyk.github.io/v1
kind: VolumeTierPolicy
metadata:
  name: %s
  namespace: %s
spec:
  mode: %s
  selector:
    matchLabels:
      app: %s
  tiers:
  - name: %s
    ceilings:
      iops: 100
      throughputMiBps: 1000
  - name: %s
    ceilings:
      iops: 100000
      throughputMiBps: 10000
  scaleUp:
    for: %s
    utilizationAbove: 80
  scaleDown:
    for: %s
    utilizationBelow: 30
`, name, ns, mode, app, base, fast, scaleFor, scaleFor)
}

// pvc returns a hostpath PVC in tierd-e2e starting on the base tier.
func pvc(name, app string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: tierd-e2e
  labels:
    app: %s
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: e2e-hostpath
  volumeAttributesClassName: e2e-base
  resources:
    requests:
      storage: 200Mi
`, name, app)
}

// pvcIn returns a PVC of StorageClass sc. A non-empty tier sets its
// VolumeAttributesClass; block makes it a raw block volume.
func pvcIn(ns, name, app, sc, tier string, block bool) string {
	extra := ""
	if tier != "" {
		extra += "\n  volumeAttributesClassName: " + tier
	}
	if block {
		extra += "\n  volumeMode: Block"
	}
	return fmt.Sprintf(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
  labels:
    app: %s
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: %s%s
  resources:
    requests:
      storage: 200Mi
`, name, ns, app, sc, extra)
}

// loadPodSpec is the fio pod on the load PVC, starting idle.
var loadPodSpec = loadPodIn(tieringNS, loadPod, loadPVC, "idle", false, true)

// loadPodIn returns a pod running fio on PVC claim, in the profile written to
// /tmp/mode (initially mode):
//   - sustained: 400 random-write IOPS, 4x the base ceiling
//   - bursty: 1s at 400 IOPS, then 11s idle; the agent's 3s window goes over
//     the base ceiling for ~5s per burst, half the `for` window
//   - anything else: idle
//
// Four jobs share the 400 IOPS, so one slow write (NFS exports are sync)
// doesn't cap the rate. A block claim is written raw at /dev/load. Without direct I/O each write is
// fsynced instead, for filesystems that don't support O_DIRECT.
func loadPodIn(ns, name, claim, mode string, block, direct bool) string {
	file, mount := "/data/fio.dat", "volumeMounts:\n    - name: data\n      mountPath: /data"
	if block {
		file, mount = "/dev/load", "volumeDevices:\n    - name: data\n      devicePath: /dev/load"
	}
	io := "--direct=1"
	if !direct {
		io = "--direct=0 --fsync=1"
	}
	return fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  # sh ignores SIGTERM; don't wait out the default 30s grace period.
  terminationGracePeriodSeconds: 0
  containers:
  - name: fio
    image: alpine:3.22
    command: ["sh", "-c"]
    args:
    - |
      apk add --no-cache fio >/dev/null || exit 1
      [ -f /tmp/mode ] || echo %[3]s > /tmp/mode
      touch /tmp/ready
      run() { fio --name=load --filename=%[4]s --size=50M --rw=randwrite --bs=4k \
        %[5]s --ioengine=psync --numjobs=4 --rate_iops=100 --time_based --runtime="$1" >/dev/null; }
      while true; do
        case "$(cat /tmp/mode 2>/dev/null)" in
          sustained) run 5 ;;
          bursty) run 1; for i in $(seq 11); do [ "$(cat /tmp/mode)" = bursty ] || break; sleep 1; done ;;
          *) sleep 1 ;;
        esac
      done
    readinessProbe:
      exec:
        command: ["test", "-f", "/tmp/ready"]
    %[6]s
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: %[7]s
`, name, ns, mode, file, io, mount, claim)
}

var _ = Describe("Tiering", Ordered, func() {
	SetDefaultEventuallyTimeout(4 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	BeforeAll(func() {
		By("creating the namespace, StorageClass and tiers")
		apply(tieringSetup)

		By("creating the load PVC, its policy and the fio pod")
		apply(policy("tiering", "e2e-tiering") + "---" + pvc(loadPVC, "e2e-tiering") + "---" + loadPodSpec)
		_, err := utils.Run(exec.Command("kubectl", "wait", "pod/"+loadPod, "-n", tieringNS,
			"--for=condition=Ready", "--timeout=5m"))
		Expect(err).NotTo(HaveOccurred(), "fio pod did not become ready")

		By("waiting for tierd to manage the load PVC")
		Eventually(func() string {
			return get("vts", loadPVC, `{.status.conditions[?(@.type=="Managed")].status}`)
		}).Should(Equal("True"))
	})

	AfterAll(func() {
		// Wait, so the fio pod is gone before other specs measure the disk.
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", tieringNS, "--timeout=2m"))
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		for _, args := range [][]string{
			{"get", "vtp,vts,volumeiosample,pvc", "-n", tieringNS, "-o", "yaml"},
			{"get", "events", "-n", tieringNS, "--sort-by=.lastTimestamp"},
			{"logs", "deployment/tierd-controller-manager", "-n", namespace, "--tail=200"},
			{"logs", "daemonset/" + agentDaemonSet, "-n", namespace, "--tail=100"},
		} {
			out, _ := utils.Run(exec.Command("kubectl", args...))
			_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n", strings.Join(args, " "), out)
		}
	})

	It("leaves a PVC matched by two policies untouched and reports Conflict on both", func() {
		apply(policy("conflict-a", "e2e-conflict") + "---" + policy("conflict-b", "e2e-conflict") +
			"---" + pvc("conflict", "e2e-conflict"))
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", tieringNS,
				"vtp/conflict-a", "vtp/conflict-b", "pvc/conflict"))
		})

		for _, p := range []string{"conflict-a", "conflict-b"} {
			Eventually(func() string {
				return get("vtp", p, `{.status.conditions[?(@.type=="Conflict")].status}`)
			}).Should(Equal("True"), "policy %s Conflict", p)
		}
		Expect(get("vts", "conflict", "{.status.policyRef}")).To(BeEmpty())
		Consistently(func() string {
			return get("pvc", "conflict", "{.spec.volumeAttributesClassName}")
		}, 5*time.Second, time.Second).Should(Equal(tierBase))
	})

	It("scales up under sustained load", func() {
		setLoad("sustained")
		Eventually(func() string { return get("pvc", loadPVC, "{.spec.volumeAttributesClassName}") }).
			Should(Equal(tierFast))
		Eventually(func() string { return get("pvc", loadPVC, "{.status.currentVolumeAttributesClassName}") }).
			Should(Equal(tierFast))
		Eventually(func() string { return get("vts", loadPVC, "{.status.currentTier}") }).
			Should(Equal(tierFast))
	})

	It("scales down when idle", func() {
		setLoad("idle")
		Eventually(func() string { return get("pvc", loadPVC, "{.status.currentVolumeAttributesClassName}") }).
			Should(Equal(tierBase))
		Eventually(func() string { return get("vts", loadPVC, "{.status.currentTier}") }).
			Should(Equal(tierBase))
	})

	It("does not flap under bursty load", func() {
		lastModified := get("vts", loadPVC, "{.status.lastModified}")
		setLoad("bursty")
		Consistently(func() string {
			return get("pvc", loadPVC, "{.spec.volumeAttributesClassName}")
		}, 30*time.Second, time.Second).Should(Equal(tierBase))
		Expect(get("vts", loadPVC, "{.status.lastModified}")).To(Equal(lastModified),
			"tierd changed tier during bursty load")
		setLoad("idle")
	})

	It("recreates a deleted VolumeTierState and restarts its `for` window", func() {
		By("loading the PVC until tierd wants to scale up")
		setLoad("sustained")
		Eventually(func() string { return get("vts", loadPVC, "{.status.pendingDirection}") }).
			Should(Equal("Up"))
		oldUID := get("vts", loadPVC, "{.metadata.uid}")

		By("deleting the VolumeTierState")
		_, err := utils.Run(exec.Command("kubectl", "delete", "vts", loadPVC, "-n", tieringNS))
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string { return get("vts", loadPVC, "{.metadata.uid}") }).
			ShouldNot(Or(BeEmpty(), Equal(oldUID)))

		By("checking the new state waits out a full window before acting")
		Consistently(func() string {
			return get("pvc", loadPVC, "{.spec.volumeAttributesClassName}")
		}, scaleFor/2, time.Second).Should(Equal(tierBase))
		Eventually(func() string { return get("pvc", loadPVC, "{.spec.volumeAttributesClassName}") }).
			Should(Equal(tierFast))
		setLoad("idle")
	})

	It("pauses on a manual tier change and resumes when the annotation is removed", func() {
		By("waiting for the idle PVC to settle on base")
		Eventually(func() string { return get("pvc", loadPVC, "{.status.currentVolumeAttributesClassName}") }).
			Should(Equal(tierBase))
		Eventually(func() string { return get("vts", loadPVC, "{.status.currentTier}") }).
			Should(Equal(tierBase))

		By("moving the PVC to fast by hand")
		kubectl("patch", "pvc", loadPVC, "-n", tieringNS, "--type=merge",
			"-p", `{"spec":{"volumeAttributesClassName":"`+tierFast+`"}}`)
		Eventually(func() string { return get("pvc", loadPVC, overridePath) }).Should(Equal("true"))
		Eventually(func() string {
			return get("vts", loadPVC, `{.status.conditions[?(@.type=="ManualOverride")].status}`)
		}).Should(Equal("True"))
		Eventually(func() string {
			return kubectl("get", "events", "-n", tieringNS, "--field-selector", "reason=ManualOverride", "-o", "name")
		}).ShouldNot(BeEmpty())

		By("checking tierd leaves the idle PVC on fast")
		Consistently(func() string {
			return get("pvc", loadPVC, "{.spec.volumeAttributesClassName}")
		}, scaleFor+5*time.Second, time.Second).Should(Equal(tierFast))

		By("removing the annotation")
		kubectl("annotate", "pvc", loadPVC, "-n", tieringNS, manualOverride+"-")
		Eventually(func() string {
			return get("vts", loadPVC, `{.status.conditions[?(@.type=="ManualOverride")].status}`)
		}).Should(Equal("False"))
		Eventually(func() string { return get("pvc", loadPVC, "{.status.currentVolumeAttributesClassName}") }).
			Should(Equal(tierBase))
	})

	It("only recommends in Recommend mode", func() {
		setMode("tiering", "Recommend")
		setLoad("sustained")
		Eventually(func() string { return get("vts", loadPVC, "{.status.recommendedTier}") }).
			Should(Equal(tierFast))
		Consistently(func() string {
			return get("pvc", loadPVC, "{.spec.volumeAttributesClassName}")
		}, 5*time.Second, time.Second).Should(Equal(tierBase))

		By("going idle before switching back, so Act doesn't apply the old recommendation")
		setLoad("idle")
		Eventually(func() string { return get("vts", loadPVC, "{.status.recommendedTier}") }).
			Should(Equal(tierBase))
		setMode("tiering", "Act")
	})

	It("rolls back a tier the driver rejects and backs off", func() {
		By("making the rejected class the policy's upper tier")
		kubectl("patch", "vtp", "tiering", "-n", tieringNS, "--type=json",
			"-p", `[{"op":"replace","path":"/spec/tiers/1/name","value":"e2e-rejected"}]`)
		DeferCleanup(func() {
			// Wait for the pending scale-up to clear: it has already waited
			// out its window and would move to fast at once.
			setLoad("idle")
			Eventually(func() string { return get("vts", loadPVC, "{.status.pendingDirection}") }).Should(BeEmpty())
			kubectl("patch", "vtp", "tiering", "-n", tieringNS, "--type=json",
				"-p", `[{"op":"replace","path":"/spec/tiers/1/name","value":"`+tierFast+`"}]`)
		})

		setLoad("sustained")
		Eventually(func() string { return get("vts", loadPVC, "{.status.infeasibleTiers[0].name}") }).
			Should(Equal("e2e-rejected"))
		Eventually(func() string { return get("pvc", loadPVC, "{.spec.volumeAttributesClassName}") }).
			Should(Equal(tierBase))
		Eventually(func() string {
			return kubectl("get", "events", "-n", tieringNS, "--field-selector", "reason=TierChangeFailed", "-o", "name")
		}).ShouldNot(BeEmpty())

		By("checking tierd doesn't retry during the backoff")
		Consistently(func() string {
			return get("pvc", loadPVC, "{.spec.volumeAttributesClassName}")
		}, scaleFor+5*time.Second, time.Second).Should(Equal(tierBase))
		Expect(get("vts", loadPVC, "{.status.reason}")).To(ContainSubstring("retrying after"))
		Expect(get("vts", loadPVC, `{.status.conditions[?(@.type=="ManualOverride")].status}`)).To(Equal("False"))
	})

	It("skips annotated PVCs and releases PVCs that stop matching", func() {
		apply(pvc("released", "e2e-tiering") + "---" + pvc("skipped", "e2e-tiering") + "---" +
			policy("doomed", "e2e-doomed") + "---" + pvc("doomed", "e2e-doomed"))
		kubectl("annotate", "pvc", "skipped", "-n", tieringNS, skipAnnotation+"=true")
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", tieringNS, "--ignore-not-found",
				"pvc/released", "pvc/skipped", "pvc/doomed", "vtp/doomed"))
		})

		Eventually(func() string { return get("vts", "released", "{.status.policyRef}") }).Should(Equal("tiering"))
		Eventually(func() string { return get("vts", "doomed", "{.status.policyRef}") }).Should(Equal("doomed"))
		Expect(get("vts", "skipped", "{.metadata.name}")).To(BeEmpty())
		Expect(get("vtp", "tiering", "{.status.pvcs}")).NotTo(ContainSubstring("skipped"))

		By("annotating a managed PVC to skip it")
		kubectl("annotate", "pvc", "released", "-n", tieringNS, skipAnnotation+"=true")
		Eventually(func() string { return get("vts", "released", "{.metadata.uid}") }).Should(BeEmpty())

		By("relabelling a managed PVC")
		kubectl("annotate", "pvc", "released", "-n", tieringNS, skipAnnotation+"-")
		Eventually(func() string { return get("vts", "released", "{.status.policyRef}") }).Should(Equal("tiering"))
		kubectl("label", "pvc", "released", "-n", tieringNS, "--overwrite", "app=e2e-none")
		Eventually(func() string { return get("vts", "released", "{.metadata.uid}") }).Should(BeEmpty())

		By("deleting a policy")
		kubectl("delete", "vtp", "doomed", "-n", tieringNS)
		Eventually(func() string { return get("vts", "doomed", "{.metadata.uid}") }).Should(BeEmpty())
	})

	It("skips PVCs of other drivers and doesn't report them as conflicts", func() {
		apply(policyIn(tieringNS, "other", "e2e-tiering", "Act", "e2e-other-base", "e2e-other-fast") + "---" +
			pvcIn(tieringNS, "other", "e2e-tiering", "e2e-other", "", false))
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "-n", tieringNS, "--ignore-not-found",
				"vtp/other", "pvc/other"))
		})

		Eventually(func() string { return get("vtp", "tiering", `{.status.conditions[?(@.type=="Ready")].message}`) }).
			Should(ContainSubstring("other (other.csi.example.com)"))
		Eventually(func() string { return get("vtp", "other", `{.status.conditions[?(@.type=="Ready")].message}`) }).
			Should(ContainSubstring(loadPVC + " (hostpath.csi.k8s.io)"))
		Expect(get("vts", "other", "{.status.policyRef}")).To(Equal("other"))
		Expect(get("vts", loadPVC, "{.status.policyRef}")).To(Equal("tiering"))
		for _, p := range []string{"tiering", "other"} {
			Expect(get("vtp", p, `{.status.conditions[?(@.type=="Conflict")].status}`)).To(Equal("False"), p)
		}
	})
})

const (
	manualOverride = "tierd.zanderyk.github.io/manual-override"
	skipAnnotation = "tierd.zanderyk.github.io/skip-volume-tier"
	overridePath   = `{.metadata.annotations.tierd\.zanderyk\.github\.io/manual-override}`
)

// kubectl runs kubectl and returns its output, failing the spec on error.
func kubectl(args ...string) string {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("kubectl", args...))
	Expect(err).NotTo(HaveOccurred())
	return out
}

// setMode switches a tierd-e2e policy's mode.
func setMode(policy, mode string) {
	GinkgoHelper()
	By("switching policy " + policy + " to " + mode)
	kubectl("patch", "vtp", policy, "-n", tieringNS, "--type=merge", "-p", `{"spec":{"mode":"`+mode+`"}}`)
}

// apply pipes manifest into kubectl apply.
func apply(manifest string) {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

// get returns a jsonpath field of a tierd-e2e object, or "" if it can't be read.
func get(kind, name, jsonpath string) string {
	return getIn(tieringNS, kind, name, jsonpath)
}

// getIn returns a jsonpath field of an object in ns, or "" if it can't be read.
func getIn(ns, kind, name, jsonpath string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", kind, name, "-n", ns, "-o", "jsonpath="+jsonpath))
	if err != nil {
		return ""
	}
	return out
}

// setLoad switches the fio pod's profile.
func setLoad(mode string) {
	GinkgoHelper()
	By("setting the load to " + mode)
	_, err := utils.Run(exec.Command("kubectl", "exec", loadPod, "-n", tieringNS, "--",
		"sh", "-c", "echo "+mode+" > /tmp/mode"))
	Expect(err).NotTo(HaveOccurred())
}
