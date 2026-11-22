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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
	"github.com/zanderyk/tierd/internal/controller"
)

const (
	tieringNS  = "tierd-e2e"
	tieringApp = "e2e-tiering"
	tierBase   = "e2e-base"
	tierFast   = "e2e-fast"
	loadPVC    = "load"
	loadPod    = "load"

	// dataVolume is the fio pod's volume.
	dataVolume = "data"

	// scaleFor is the policies' scale-up and scale-down `for` window.
	scaleFor = 10 * time.Second
)

// newPolicy returns a policy selecting app=<app> over tiers base and fast.
// base tops out at 100 IOPS and fast at 100k; throughput ceilings are high so
// IOPS alone decides.
func newPolicy(ns, name, app string, mode tierdv1.VolumeTierPolicyMode, base, fast string) *tierdv1.VolumeTierPolicy {
	return &tierdv1.VolumeTierPolicy{
		Name: name, Namespace: ns,
		Spec: tierdv1.VolumeTierPolicySpec{
			Mode:     mode,
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": app}},
			Tiers: []tierdv1.Tier{
				{VACName: base, Ceilings: &tierdv1.Ceiling{IOPS: new(int64(100)), ThroughputMiBPS: new(int64(1000))}},
				{VACName: fast, Ceilings: &tierdv1.Ceiling{IOPS: new(int64(100000)), ThroughputMiBPS: new(int64(10000))}},
			},
			ScaleUp:   tierdv1.ScaleUp{For: metav1.Duration{Duration: scaleFor}, UtilizationAbove: new(int32(80))},
			ScaleDown: &tierdv1.ScaleDown{For: metav1.Duration{Duration: scaleFor}, UtilizationBelow: 30},
		},
	}
}

// hostpathPolicy is an Act-mode policy in tierd-e2e over the hostpath tiers.
func hostpathPolicy(name, app string) *tierdv1.VolumeTierPolicy {
	return newPolicy(tieringNS, name, app, tierdv1.VolumeTierPolicyModeAct, tierBase, tierFast)
}

// newPVC returns a 200Mi PVC of StorageClass sc. A non-empty tier sets its
// VolumeAttributesClass; block makes it a raw block volume.
func newPVC(ns, name, app, sc, tier string, block bool) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		Name: name, Namespace: ns, Labels: map[string]string{"app": app},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("200Mi")},
			},
		},
	}
	if tier != "" {
		pvc.Spec.VolumeAttributesClassName = &tier
	}
	if block {
		pvc.Spec.VolumeMode = new(corev1.PersistentVolumeBlock)
	}
	return pvc
}

// hostpathPVC is a hostpath PVC in tierd-e2e starting on the base tier.
func hostpathPVC(name, app string) *corev1.PersistentVolumeClaim {
	return newPVC(tieringNS, name, app, "e2e-hostpath", tierBase, false)
}

// fioScript runs fio in the profile written to /tmp/mode, initially %[1]s:
//   - sustained: 400 random-write IOPS, 4x the base ceiling
//   - bursty: 1s at 400 IOPS, then 11s idle; the agent's 3s window goes over
//     the base ceiling for ~5s per burst, half the `for` window
//   - anything else: idle
//
// Four jobs share the 400 IOPS, so one slow write doesn't cap the rate.
const fioScript = `apk add --no-cache fio >/dev/null || exit 1
[ -f /tmp/mode ] || echo %[1]s > /tmp/mode
touch /tmp/ready
run() { fio --name=load --filename=%[2]s --size=50M --rw=randwrite --bs=4k %[3]s \
  --ioengine=psync --numjobs=4 --rate_iops=100 --time_based --runtime="$1" >/dev/null; }
while true; do
  case "$(cat /tmp/mode 2>/dev/null)" in
    sustained) run 5 ;;
    bursty) run 1; for i in $(seq 11); do [ "$(cat /tmp/mode)" = bursty ] || break; sleep 1; done ;;
    *) sleep 1 ;;
  esac
done`

// newLoadPod returns a pod running fio on PVC claim, starting in mode. A
// block claim is written raw at /dev/load. Without direct I/O each write is
// fsynced instead, for filesystems that don't support O_DIRECT.
func newLoadPod(ns, name, claim, mode string, block, direct bool) *corev1.Pod {
	file, io := "/data/fio.dat", "--direct=1"
	if block {
		file = "/dev/load"
	}
	if !direct {
		io = "--direct=0 --fsync=1"
	}
	c := corev1.Container{
		Name:    "fio",
		Image:   "alpine:3.22",
		Command: []string{"sh", "-c", fmt.Sprintf(fioScript, mode, file, io)},
		ReadinessProbe: &corev1.Probe{
			Exec: &corev1.ExecAction{Command: []string{"test", "-f", "/tmp/ready"}}},
	}
	if block {
		c.VolumeDevices = []corev1.VolumeDevice{{Name: dataVolume, DevicePath: file}}
	} else {
		c.VolumeMounts = []corev1.VolumeMount{{Name: dataVolume, MountPath: "/data"}}
	}
	return &corev1.Pod{
		Name: name, Namespace: ns,
		Spec: corev1.PodSpec{
			// sh ignores SIGTERM; don't wait out the default 30s grace period.
			TerminationGracePeriodSeconds: new(int64(0)),
			Containers:                    []corev1.Container{c},
			Volumes: []corev1.Volume{{Name: dataVolume,
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}},
		},
	}
}

var _ = Describe("Tiering", Ordered, func() {
	SetDefaultEventuallyTimeout(4 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	ns := &corev1.Namespace{Name: tieringNS}

	BeforeAll(func() {
		By("creating the load PVC, its policy and the fio pod")
		create(ns, hostpathPolicy("tiering", tieringApp), hostpathPVC(loadPVC, tieringApp),
			newLoadPod(tieringNS, loadPod, loadPVC, "idle", false, true))
		Eventually(func() bool { return podReady(tieringNS, loadPod) }, 5*time.Minute).
			Should(BeTrue(), "fio pod did not become ready")

		By("waiting for tierd to manage the load PVC")
		Eventually(vtsCond(loadPVC, controller.ConditionVTSManaged)).
			Should(HaveField("Status", metav1.ConditionTrue))
	})

	AfterAll(func() {
		// Wait, so the fio pod is gone before other specs measure the disk.
		removeAndWait(ns)
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics(tieringNS)
		}
	})

	It("leaves a PVC matched by two policies untouched and reports Conflict on both", func() {
		objs := []client.Object{hostpathPolicy("conflict-a", "e2e-conflict"),
			hostpathPolicy("conflict-b", "e2e-conflict"), hostpathPVC("conflict", "e2e-conflict")}
		create(objs...)
		DeferCleanup(func() { remove(objs...) })

		for _, p := range []string{"conflict-a", "conflict-b"} {
			Eventually(vtpCond(p, "Conflict")).Should(HaveField("Status", metav1.ConditionTrue), "policy %s", p)
		}
		Expect(vts("conflict").Status.PolicyRef).To(BeEmpty())
		Consistently(spec("conflict"), 5*time.Second, time.Second).Should(Equal(tierBase))
	})

	It("scales up under sustained load", func() {
		setLoad("sustained")
		Eventually(spec(loadPVC)).Should(Equal(tierFast))
		Eventually(loadCurrent).Should(Equal(tierFast))
		Eventually(stateTier(loadPVC)).Should(Equal(tierFast))
	})

	It("scales down when idle", func() {
		setLoad("idle")
		Eventually(loadCurrent).Should(Equal(tierBase))
		Eventually(stateTier(loadPVC)).Should(Equal(tierBase))
	})

	It("does not flap under bursty load", func() {
		lastModified := vts(loadPVC).Status.LastModified
		setLoad("bursty")
		Consistently(spec(loadPVC), 30*time.Second, time.Second).Should(Equal(tierBase))
		Expect(vts(loadPVC).Status.LastModified).To(Equal(lastModified), "tierd changed tier during bursty load")
		setLoad("idle")
	})

	It("recreates a deleted VolumeTierState and restarts its `for` window", func() {
		By("loading the PVC until tierd wants to scale up")
		setLoad("sustained")
		Eventually(pending(loadPVC)).Should(Equal(tierdv1.ScaleDirectionUp))
		old := vts(loadPVC)

		By("deleting the VolumeTierState")
		Expect(k8s.Delete(ctx, old)).To(Succeed())
		Eventually(func() string { return string(vts(loadPVC).UID) }).
			ShouldNot(Or(BeEmpty(), Equal(string(old.UID))))

		By("checking the new state waits out a full window before acting")
		Consistently(spec(loadPVC), scaleFor/2, time.Second).Should(Equal(tierBase))
		Eventually(spec(loadPVC)).Should(Equal(tierFast))
		setLoad("idle")
	})

	It("pauses on a manual tier change and resumes when the annotation is removed", func() {
		By("waiting for the idle PVC to settle on base")
		Eventually(loadCurrent).Should(Equal(tierBase))
		Eventually(stateTier(loadPVC)).Should(Equal(tierBase))

		By("moving the PVC to fast by hand")
		update(pvcRef(loadPVC), func(p *corev1.PersistentVolumeClaim) {
			p.Spec.VolumeAttributesClassName = new(tierFast)
		})
		Eventually(func() string {
			return getPVC(tieringNS, loadPVC).Annotations[controller.ManualOverrideAnnotation]
		}).Should(Equal("true"))
		Eventually(vtsCond(loadPVC, controller.ConditionVTSManualOverride)).
			Should(HaveField("Status", metav1.ConditionTrue))
		Eventually(func() int { return events(tieringNS, "ManualOverride") }).Should(BeNumerically(">", 0))

		By("checking tierd leaves the idle PVC on fast")
		Consistently(spec(loadPVC), scaleFor+5*time.Second, time.Second).Should(Equal(tierFast))

		By("removing the annotation")
		update(pvcRef(loadPVC), func(p *corev1.PersistentVolumeClaim) {
			delete(p.Annotations, controller.ManualOverrideAnnotation)
		})
		Eventually(vtsCond(loadPVC, controller.ConditionVTSManualOverride)).
			Should(HaveField("Status", metav1.ConditionFalse))
		Eventually(loadCurrent).Should(Equal(tierBase))
	})

	It("only recommends in Recommend mode", func() {
		setMode("tiering", tierdv1.VolumeTierPolicyModeRecommend)
		setLoad("sustained")
		Eventually(recommended(loadPVC)).Should(Equal(tierFast))
		Consistently(spec(loadPVC), 5*time.Second, time.Second).Should(Equal(tierBase))

		By("going idle before switching back, so Act doesn't apply the old recommendation")
		setLoad("idle")
		Eventually(recommended(loadPVC)).Should(Equal(tierBase))
		setMode("tiering", tierdv1.VolumeTierPolicyModeAct)
	})

	It("rolls back a tier the driver rejects and backs off", func() {
		setUpperTier := func(tier string) {
			By("making " + tier + " the policy's upper tier")
			update(vtpRef("tiering"), func(p *tierdv1.VolumeTierPolicy) { p.Spec.Tiers[1].VACName = tier })
		}
		setUpperTier("e2e-rejected")
		DeferCleanup(func() {
			// Wait for the pending scale-up to clear: it has already waited
			// out its window and would move to fast at once.
			setLoad("idle")
			Eventually(pending(loadPVC)).Should(BeEmpty())
			setUpperTier(tierFast)
		})

		setLoad("sustained")
		Eventually(func() []tierdv1.InfeasibleTier { return vts(loadPVC).Status.InfeasibleTiers }).
			Should(ContainElement(HaveField("Name", "e2e-rejected")))
		Eventually(spec(loadPVC)).Should(Equal(tierBase))
		Eventually(func() int { return events(tieringNS, "TierChangeFailed") }).Should(BeNumerically(">", 0))

		By("checking tierd doesn't retry during the backoff")
		Consistently(spec(loadPVC), scaleFor+5*time.Second, time.Second).Should(Equal(tierBase))
		st := vts(loadPVC)
		Expect(st.Status.Reason).To(ContainSubstring("retrying after"))
		Expect(condition(st.Status.Conditions, controller.ConditionVTSManualOverride).Status).
			To(Equal(metav1.ConditionFalse))
	})

	It("skips annotated PVCs and releases PVCs that stop matching", func() {
		skipped := hostpathPVC("skipped", tieringApp)
		skipped.Annotations = map[string]string{controller.SkipVolumeTierStateAnnotation: "true"}
		objs := []client.Object{hostpathPVC("released", tieringApp), skipped,
			hostpathPolicy("doomed", "e2e-doomed"), hostpathPVC("doomed", "e2e-doomed")}
		create(objs...)
		DeferCleanup(func() { remove(objs...) })

		Eventually(policyRef("released")).Should(Equal("tiering"))
		Eventually(policyRef("doomed")).Should(Equal("doomed"))
		Expect(vtsGone("skipped")()).To(BeTrue())
		Expect(vtp("tiering").Status.PVCs).NotTo(ContainElement("skipped"))

		setSkip := func(skip bool) {
			update(pvcRef("released"), func(p *corev1.PersistentVolumeClaim) {
				if skip {
					metav1.SetMetaDataAnnotation(&p.ObjectMeta, controller.SkipVolumeTierStateAnnotation, "true")
				} else {
					delete(p.Annotations, controller.SkipVolumeTierStateAnnotation)
				}
			})
		}

		By("annotating a managed PVC to skip it")
		setSkip(true)
		Eventually(vtsGone("released")).Should(BeTrue())

		By("relabelling a managed PVC")
		setSkip(false)
		Eventually(policyRef("released")).Should(Equal("tiering"))
		update(pvcRef("released"), func(p *corev1.PersistentVolumeClaim) { p.Labels["app"] = "e2e-none" })
		Eventually(vtsGone("released")).Should(BeTrue())

		By("deleting a policy")
		Expect(k8s.Delete(ctx, vtpRef("doomed"))).To(Succeed())
		Eventually(vtsGone("doomed")).Should(BeTrue())
	})

	It("skips PVCs of other drivers and doesn't report them as conflicts", func() {
		objs := []client.Object{
			newPolicy(tieringNS, "other", tieringApp, tierdv1.VolumeTierPolicyModeAct, "e2e-other-base", "e2e-other-fast"),
			newPVC(tieringNS, "other", tieringApp, "e2e-other", "", false),
		}
		create(objs...)
		DeferCleanup(func() { remove(objs...) })

		Eventually(vtpCond("tiering", "Ready")).
			Should(HaveField("Message", ContainSubstring("other (other.csi.example.com)")))
		Eventually(vtpCond("other", "Ready")).
			Should(HaveField("Message", ContainSubstring(loadPVC+" (hostpath.csi.k8s.io)")))
		Expect(vts("other").Status.PolicyRef).To(Equal("other"))
		Expect(vts(loadPVC).Status.PolicyRef).To(Equal("tiering"))
		for _, p := range []string{"tiering", "other"} {
			Expect(vtpCond(p, "Conflict")().Status).To(Equal(metav1.ConditionFalse), p)
		}
	})
})

// Readers of tierd-e2e objects. Those returning funcs plug into Eventually
// and Consistently.

func vts(name string) *tierdv1.VolumeTierState  { return getVTS(tieringNS, name) }
func vtp(name string) *tierdv1.VolumeTierPolicy { return getVTP(tieringNS, name) }

func spec(pvc string) func() string { return func() string { return specTier(tieringNS, pvc) } }

// loadCurrent is the load PVC's applied tier.
func loadCurrent() string { return currentTier(tieringNS, loadPVC) }

func stateTier(name string) func() string {
	return func() string { return vts(name).Status.CurrentTier }
}
func recommended(name string) func() string {
	return func() string { return vts(name).Status.RecommendedTier }
}
func policyRef(name string) func() string { return func() string { return vts(name).Status.PolicyRef } }

func pending(name string) func() tierdv1.ScaleDirection {
	return func() tierdv1.ScaleDirection { return vts(name).Status.PendingDirection }
}

func vtsCond(name, typ string) func() metav1.Condition {
	return func() metav1.Condition { return condition(vts(name).Status.Conditions, typ) }
}

func vtpCond(name, typ string) func() metav1.Condition {
	return func() metav1.Condition { return condition(vtp(name).Status.Conditions, typ) }
}

// pvcRef and vtpRef are empty objects naming a tierd-e2e PVC or policy, for
// update and delete.
func pvcRef(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{Namespace: tieringNS, Name: name}
}

func vtpRef(name string) *tierdv1.VolumeTierPolicy {
	return &tierdv1.VolumeTierPolicy{Namespace: tieringNS, Name: name}
}

func vtsGone(name string) func() bool {
	return func() bool { return gone(tieringNS, name, &tierdv1.VolumeTierState{}) }
}

// setLoad switches the tierd-e2e fio pod's profile.
func setLoad(mode string) {
	GinkgoHelper()
	By("setting the load to " + mode)
	Expect(execIn(tieringNS, loadPod, "sh", "-c", "echo "+mode+" > /tmp/mode")).To(Succeed())
}

// setMode switches a tierd-e2e policy's mode.
func setMode(policy string, mode tierdv1.VolumeTierPolicyMode) {
	GinkgoHelper()
	By(fmt.Sprintf("switching policy %s to %s", policy, mode))
	update(vtpRef(policy), func(p *tierdv1.VolumeTierPolicy) { p.Spec.Mode = mode })
}
