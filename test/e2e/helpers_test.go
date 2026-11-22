//go:build e2e

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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
	"github.com/zanderyk/tierd/test/utils"
)

// fieldOwner owns the fields the suite applies.
const fieldOwner = client.FieldOwner("tierd-e2e")

var (
	ctx = context.Background()
	// k8s is an uncached client for the kind cluster in $KUBECONFIG.
	k8s client.Client
	// clientset serves pod exec and logs, which controller-runtime lacks.
	clientset *kubernetes.Clientset
	restCfg   *rest.Config
)

// setupClient builds the clients from $KUBECONFIG.
func setupClient() {
	var err error
	restCfg, err = config.GetConfig()
	Expect(err).NotTo(HaveOccurred(), "Failed to load kubeconfig")

	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(tierdv1.AddToScheme(scheme)).To(Succeed())

	k8s, err = client.New(restCfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	clientset, err = kubernetes.NewForConfig(restCfg)
	Expect(err).NotTo(HaveOccurred())
}

// applyFile server-side applies a YAML file, relative to the project root.
func applyFile(path string) {
	GinkgoHelper()
	dir, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())
	data, err := os.ReadFile(filepath.Join(dir, path))
	Expect(err).NotTo(HaveOccurred())
	applyYAML(data)
}

// applyURL server-side applies a YAML file from the web.
func applyURL(url string) {
	GinkgoHelper()
	resp, err := http.Get(url) //nolint:gosec // pinned upstream manifests
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	Expect(resp.StatusCode).To(Equal(http.StatusOK), url)
	data, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	applyYAML(data)
}

// applyYAML server-side applies every object in a multi-document YAML.
func applyYAML(data []byte) {
	GinkgoHelper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		u := &unstructured.Unstructured{}
		err := dec.Decode(&u.Object)
		if errors.Is(err, io.EOF) {
			return
		}
		Expect(err).NotTo(HaveOccurred())
		if len(u.Object) == 0 {
			continue
		}
		Expect(k8s.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), fieldOwner, client.ForceOwnership)).
			To(Succeed(), "apply %s %s", u.GetKind(), u.GetName())
	}
}

// create creates objs, failing the spec on error.
func create(objs ...client.Object) {
	GinkgoHelper()
	for _, o := range objs {
		Expect(k8s.Create(ctx, o)).To(Succeed(), "create %T %s", o, o.GetName())
	}
}

// remove deletes objs, ignoring ones already gone.
func remove(objs ...client.Object) {
	for _, o := range objs {
		_ = k8s.Delete(ctx, o)
	}
}

// removeAndWait deletes objs and waits until they're gone.
func removeAndWait(objs ...client.Object) {
	GinkgoHelper()
	remove(objs...)
	for _, o := range objs {
		Eventually(func() bool { return gone(o.GetNamespace(), o.GetName(), o) }, 2*time.Minute).
			Should(BeTrue(), "%T %s not deleted", o, o.GetName())
	}
}

// update re-reads obj, applies mutate and writes it back, retrying on
// conflicts.
func update[T client.Object](obj T, mutate func(T)) {
	GinkgoHelper()
	Eventually(func() error {
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return err
		}
		mutate(obj)
		return k8s.Update(ctx, obj)
	}).Should(Succeed(), "update %T %s", obj, obj.GetName())
}

// fetch reads ns/name into obj and returns it; on error obj stays empty.
func fetch[T client.Object](ns, name string, obj T) T {
	_ = k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj)
	return obj
}

// gone reports whether ns/name of obj's kind doesn't exist.
func gone(ns, name string, obj client.Object) bool {
	return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj))
}

func getPVC(ns, name string) *corev1.PersistentVolumeClaim {
	return fetch(ns, name, &corev1.PersistentVolumeClaim{})
}

func getVTS(ns, name string) *tierdv1.VolumeTierState {
	return fetch(ns, name, &tierdv1.VolumeTierState{})
}

func getVTP(ns, name string) *tierdv1.VolumeTierPolicy {
	return fetch(ns, name, &tierdv1.VolumeTierPolicy{})
}

func getSample(ns, name string) *tierdv1.VolumeIOSample {
	return fetch(ns, name, &tierdv1.VolumeIOSample{})
}

// specTier is the PVC's requested VolumeAttributesClass.
func specTier(ns, name string) string {
	return ptr.Deref(getPVC(ns, name).Spec.VolumeAttributesClassName, "")
}

// currentTier is the VolumeAttributesClass the driver has applied.
func currentTier(ns, name string) string {
	return ptr.Deref(getPVC(ns, name).Status.CurrentVolumeAttributesClassName, "")
}

// condition returns the condition of type typ, or a zero one.
func condition(conds []metav1.Condition, typ string) metav1.Condition {
	if c := meta.FindStatusCondition(conds, typ); c != nil {
		return *c
	}
	return metav1.Condition{}
}

// events counts the events in ns with the given reason.
func events(ns, reason string) int {
	list := &corev1.EventList{}
	if err := k8s.List(ctx, list, client.InNamespace(ns), client.MatchingFields{"reason": reason}); err != nil {
		return 0
	}
	return len(list.Items)
}

// podReady reports whether a pod's Ready condition is true.
func podReady(ns, name string) bool {
	pod := fetch(ns, name, &corev1.Pod{})
	return slices.ContainsFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
	})
}

// waitRolledOut waits for a Deployment, DaemonSet or StatefulSet to finish
// rolling out.
func waitRolledOut(obj client.Object) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
		switch o := obj.(type) {
		case *appsv1.Deployment:
			want := ptr.Deref(o.Spec.Replicas, 1)
			g.Expect(o.Status.ObservedGeneration).To(Equal(o.Generation))
			g.Expect(o.Status.Replicas).To(Equal(want))
			g.Expect(o.Status.UpdatedReplicas).To(Equal(want))
			g.Expect(o.Status.AvailableReplicas).To(Equal(want))
		case *appsv1.DaemonSet:
			want := o.Status.DesiredNumberScheduled
			g.Expect(o.Status.ObservedGeneration).To(Equal(o.Generation))
			g.Expect(o.Status.UpdatedNumberScheduled).To(Equal(want))
			g.Expect(o.Status.NumberAvailable).To(Equal(want))
		case *appsv1.StatefulSet:
			want := ptr.Deref(o.Spec.Replicas, 1)
			g.Expect(o.Status.ObservedGeneration).To(Equal(o.Generation))
			g.Expect(o.Status.UpdatedReplicas).To(Equal(want))
			g.Expect(o.Status.ReadyReplicas).To(Equal(want))
		default:
			Fail(fmt.Sprintf("can't wait for a rollout of %T", obj))
		}
	}, 5*time.Minute, time.Second).Should(Succeed(), "%T %s did not roll out", obj, obj.GetName())
}

// execIn runs cmd in a pod's first container.
func execIn(ns, pod string, cmd ...string) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Command: cmd, Stdout: true, Stderr: true}, clientgoscheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(restCfg, http.MethodPost, req.URL())
	if err != nil {
		return err
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return fmt.Errorf("exec %s in %s/%s: %w: %s", strings.Join(cmd, " "), ns, pod, err, stderr.String())
	}
	return nil
}

// podLogs returns the last lines of a tierd-system pod's log, or 0 for all
// of it.
func podLogs(pod string, tail int64) (string, error) {
	opts := &corev1.PodLogOptions{}
	if tail > 0 {
		opts.TailLines = &tail
	}
	out, err := clientset.CoreV1().Pods(namespace).GetLogs(pod, opts).DoRaw(ctx)
	return string(out), err
}

// dumpDiagnostics prints the tierd objects, PVCs, pods and events in ns,
// and the manager and agent logs, for a failed spec.
func dumpDiagnostics(ns string) {
	for _, list := range []client.ObjectList{
		&tierdv1.VolumeTierPolicyList{}, &tierdv1.VolumeTierStateList{}, &tierdv1.VolumeIOSampleList{},
		&corev1.PersistentVolumeClaimList{}, &corev1.PodList{},
	} {
		if err := k8s.List(ctx, list, client.InNamespace(ns)); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "list %T: %v\n", list, err)
			continue
		}
		out, _ := yaml.Marshal(list)
		_, _ = fmt.Fprintf(GinkgoWriter, "%T in %s:\n%s\n", list, ns, out)
	}

	evs := &corev1.EventList{}
	if err := k8s.List(ctx, evs, client.InNamespace(ns)); err == nil {
		slices.SortFunc(evs.Items, func(a, b corev1.Event) int {
			return eventTime(a).Compare(eventTime(b))
		})
		_, _ = fmt.Fprintf(GinkgoWriter, "Events in %s:\n", ns)
		for _, e := range evs.Items {
			_, _ = fmt.Fprintf(GinkgoWriter, "  %s %s %s/%s %s: %s\n", eventTime(e).Format(time.TimeOnly),
				e.Type, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, e.Message)
		}
	}

	for _, labels := range []client.MatchingLabels{
		{"control-plane": "controller-manager"},
		{"app.kubernetes.io/component": "agent"},
	} {
		pods := &corev1.PodList{}
		if err := k8s.List(ctx, pods, client.InNamespace(namespace), labels); err != nil {
			continue
		}
		for _, p := range pods.Items {
			out, err := podLogs(p.Name, 200)
			if err != nil {
				out = err.Error()
			}
			_, _ = fmt.Fprintf(GinkgoWriter, "Logs of %s:\n%s\n", p.Name, out)
		}
	}
}

// eventTime is when an event last happened, whichever API wrote it.
func eventTime(e corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case e.Series != nil:
		return e.Series.LastObservedTime.Time
	}
	return e.EventTime.Time
}
