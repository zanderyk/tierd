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

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
	"github.com/zanderyk/tierd/internal/agent/procfs"
)

const (
	testNS     = "default"
	testPVC    = "data"
	testPV     = "pvc-a"
	testPVCUID = types.UID("uid-data")
	testPod    = "aaaa-1"
	testDevice = "8:32"
)

var testScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := tierdv1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}()

// newFakeClient returns a fake API server holding objs, with funcs
// intercepting its calls.
func newFakeClient(funcs interceptor.Funcs, objs ...client.Object) client.WithWatch {
	return interceptor.NewClient(fake.NewClientBuilder().WithScheme(testScheme).WithObjects(objs...).
		WithStatusSubresource(&tierdv1.VolumeIOSample{}, &tierdv1.VolumeTierState{}).Build(), funcs)
}

// newPublishCollector returns a collector on node-1 that has discovered one
// block volume, testPV, mounted by testPod, with a full window. The pod's
// cgroup reports io pressure.
func newPublishCollector(t *testing.T, c client.Client) *Collector {
	t.Helper()
	t.Cleanup(func() { DeleteVolumeMetrics(testPV) })

	cgroups := t.TempDir()
	pod := filepath.Join(cgroups, "kubepods", "pod"+testPod)
	if err := os.MkdirAll(pod, 0o755); err != nil {
		t.Fatal(err)
	}
	pressure, err := os.ReadFile(filepath.Join("procfs", "testdata", "io.pressure"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pod, "io.pressure"), pressure, 0o644); err != nil {
		t.Fatal(err)
	}

	col := &Collector{
		Client:  c,
		Reader:  c,
		Config:  Config{NodeName: "node-1", CgroupRoot: cgroups, Window: time.Minute},
		volumes: map[string]volume{testPV: {pvName: testPV, kind: kindBlock, mount: procfs.Mount{Device: testDevice}, podUIDs: []string{testPod}}},
		prev:    map[string]reading{},
		windows: map[string]*Window{},
		claims:  map[string]claim{},
		removed: map[string]bool{},
	}
	fillWindow(col, testDevice, 250)
	return col
}

// fillWindow replaces device's window with three 10s intervals at iops.
func fillWindow(c *Collector, device string, iops float64) {
	w := NewWindow(c.Config.Window)
	for range 3 {
		w.Add(procfs.Interval{
			Elapsed:        10 * time.Second,
			IOPS:           iops,
			BytesPerSecond: 1_000_000,
			AvgLatency:     4 * time.Millisecond,
			Utilization:    0.5,
			QueueDepth:     1.5,
			HasLatency:     true,
			HasUtilization: true,
		})
	}
	c.windows[device] = w
}

func boundPV() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		Name: testPV,
		Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{
			Namespace: testNS, Name: testPVC, UID: testPVCUID,
		}},
	}
}

// tierState is testPVC's VolumeTierState, assigned to policy (empty: not
// managed).
func tierState(policy string) *tierdv1.VolumeTierState {
	return &tierdv1.VolumeTierState{
		Name: testPVC, Namespace: testNS,
		Status: tierdv1.VolumeTierStateStatus{PolicyRef: policy},
	}
}

// sampleOwnedBy is an existing VolumeIOSample of the PVC with uid.
func sampleOwnedBy(uid types.UID) *tierdv1.VolumeIOSample {
	return &tierdv1.VolumeIOSample{
		Name: testPVC, Namespace: testNS,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: testPVC, UID: uid,
		}},
	}
}

// getSample returns testPVC's VolumeIOSample, or nil if there is none.
func getSample(t *testing.T, c client.Client) *tierdv1.VolumeIOSample {
	t.Helper()
	s := &tierdv1.VolumeIOSample{}
	err := c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: testPVC}, s)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPublishWritesSample(t *testing.T) {
	c := newFakeClient(interceptor.Funcs{}, boundPV(), tierState("fast"))
	col := newPublishCollector(t, c)

	col.publish(context.Background())

	s := getSample(t, c)
	if s == nil {
		t.Fatal("no VolumeIOSample created")
	}
	if !ownedBy(s, testPVCUID) {
		t.Errorf("owner references = %+v, want the PVC", s.OwnerReferences)
	}
	st := s.Status
	if st.NodeName != "node-1" || st.PVName != testPV || st.Device != testDevice || st.Samples != 3 ||
		st.Window.Duration != 30*time.Second || st.IOPS != 250 || st.ThroughputBytesPerSecond != 1_000_000 {
		t.Errorf("status = %+v", st)
	}
	if st.LatencyP50Micros == nil || *st.LatencyP50Micros != 4000 ||
		st.LatencyP95Micros == nil || *st.LatencyP95Micros != 4000 {
		t.Errorf("latency = %v/%v, want 4000µs", st.LatencyP50Micros, st.LatencyP95Micros)
	}
	if st.UtilizationPercent == nil || *st.UtilizationPercent != 50 {
		t.Errorf("utilization = %v, want 50", st.UtilizationPercent)
	}
	if st.QueueDepth == nil || st.QueueDepth.MilliValue() != 1500 {
		t.Errorf("queue depth = %v, want 1.5", st.QueueDepth)
	}
	// The highest avg60 in testdata/io.pressure's "some" line.
	if st.IOPressureSomeAvg60 == nil || st.IOPressureSomeAvg60.MilliValue() != 34250 {
		t.Errorf("io pressure = %v, want 34.25", st.IOPressureSomeAvg60)
	}
	if cond := meta.FindStatusCondition(st.Conditions, tierdv1.VolumeIOSampleMeasured); cond == nil ||
		cond.Reason != tierdv1.MeasuredComplete {
		t.Errorf("Measured condition = %+v, want %s", cond, tierdv1.MeasuredComplete)
	}
	if got := seriesCount(t); got != len(volumeGauges) {
		t.Errorf("%d metric series, want %d", got, len(volumeGauges))
	}

	// The next publish updates the same sample.
	fillWindow(col, testDevice, 900)
	col.publish(context.Background())
	if got := getSample(t, c).Status.IOPS; got != 900 {
		t.Errorf("after second publish: IOPS = %d, want 900", got)
	}
}

func TestPublishSkipsVolumes(t *testing.T) {
	unbound := boundPV()
	unbound.Spec.ClaimRef = nil
	tests := []struct {
		name  string
		objs  []client.Object
		funcs interceptor.Funcs
		// noWindow drops the volume's window, as before its second sample.
		noWindow bool
	}{
		{name: "window not full yet", objs: []client.Object{boundPV(), tierState("fast")}, noWindow: true},
		{name: "PV gone", objs: []client.Object{tierState("fast")}},
		{name: "PV not bound", objs: []client.Object{unbound, tierState("fast")}},
		{name: "PV unreadable", objs: []client.Object{boundPV(), tierState("fast")},
			funcs: failGet[*corev1.PersistentVolume]()},
		{name: "VolumeTierState unreadable", objs: []client.Object{boundPV(), tierState("fast")},
			funcs: failGet[*tierdv1.VolumeTierState]()},
		{name: "no VolumeTierState", objs: []client.Object{boundPV()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeClient(tt.funcs, tt.objs...)
			col := newPublishCollector(t, c)
			if tt.noWindow {
				delete(col.windows, testDevice)
			}

			col.publish(context.Background())

			if s := getSample(t, c); s != nil {
				t.Errorf("VolumeIOSample created: %+v", s.Status)
			}
			if got := seriesCount(t); got != 0 {
				t.Errorf("%d metric series, want 0", got)
			}
		})
	}
}

// failGet makes Gets of T fail.
func failGet[T client.Object]() interceptor.Funcs {
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(T); ok {
			return errors.New("injected")
		}
		return c.Get(ctx, key, obj, opts...)
	}}
}

func TestPublishRemovesSampleOfUnmanagedPVC(t *testing.T) {
	state := tierState("")
	deletes := 0
	c := newFakeClient(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch,
		obj client.Object, opts ...client.DeleteOption) error {
		deletes++
		return c.Delete(ctx, obj, opts...)
	}}, boundPV(), state, sampleOwnedBy(testPVCUID))
	col := newPublishCollector(t, c)
	SetVolumeMetrics(claim{namespace: testNS, name: testPVC}, testPV, "node-1", VolumeMetrics{}, nil)

	col.publish(context.Background())
	if s := getSample(t, c); s != nil {
		t.Fatal("VolumeIOSample of unmanaged PVC not deleted")
	}
	if got := seriesCount(t); got != 0 {
		t.Errorf("%d metric series, want 0", got)
	}

	// The delete isn't repeated.
	col.publish(context.Background())
	if deletes != 1 {
		t.Errorf("%d deletes, want 1", deletes)
	}

	// Managed again: the sample comes back.
	state.Status.PolicyRef = "fast"
	if err := c.Status().Update(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	col.publish(context.Background())
	if getSample(t, c) == nil {
		t.Error("VolumeIOSample not recreated once the PVC is managed again")
	}
	if col.removed[testPV] {
		t.Error("PV still marked removed")
	}
}

func TestPublishRetriesFailedDelete(t *testing.T) {
	failing := true
	c := newFakeClient(interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch,
		obj client.Object, opts ...client.DeleteOption) error {
		if failing {
			return errors.New("injected")
		}
		return c.Delete(ctx, obj, opts...)
	}}, boundPV(), tierState(""), sampleOwnedBy(testPVCUID))
	col := newPublishCollector(t, c)

	col.publish(context.Background())
	if getSample(t, c) == nil || col.removed[testPV] {
		t.Fatal("failed delete treated as done")
	}

	failing = false
	col.publish(context.Background())
	if getSample(t, c) != nil {
		t.Error("VolumeIOSample not deleted on retry")
	}
}

func TestPublishLeavesSampleOfPreviousPVC(t *testing.T) {
	c := newFakeClient(interceptor.Funcs{}, boundPV(), tierState("fast"), sampleOwnedBy("uid-previous"))
	col := newPublishCollector(t, c)

	col.publish(context.Background())

	if st := getSample(t, c).Status; st.NodeName != "" || st.IOPS != 0 {
		t.Errorf("status of the previous PVC's sample written: %+v", st)
	}
}

func TestPublishUnmeasurableVolume(t *testing.T) {
	c := newFakeClient(interceptor.Funcs{}, boundPV(), tierState("fast"))
	col := newPublishCollector(t, c)
	col.volumes[testPV] = volume{pvName: testPV, kind: kindNone, mount: procfs.Mount{Device: "0:99", FSType: "fuse.blobfuse2"}}

	col.publish(context.Background())

	s := getSample(t, c)
	if s == nil {
		t.Fatal("no VolumeIOSample created")
	}
	if cond := meta.FindStatusCondition(s.Status.Conditions, tierdv1.VolumeIOSampleMeasured); cond == nil ||
		cond.Reason != tierdv1.MeasuredNoSource || cond.Status != metav1.ConditionFalse {
		t.Errorf("Measured condition = %+v, want False/%s", cond, tierdv1.MeasuredNoSource)
	}
	if s.Status.LatencyP95Micros != nil || s.Status.UtilizationPercent != nil {
		t.Errorf("figures reported for an unmeasurable volume: %+v", s.Status)
	}
	if got := seriesCount(t); got != 0 {
		t.Errorf("%d metric series, want 0", got)
	}
}

// When another writer creates the sample between the agent's Get and Create,
// the agent reads it back and writes its status.
func TestWriteSampleCreateRace(t *testing.T) {
	sampleKind := schema.GroupResource{Group: tierdv1.GroupVersion.Group, Resource: "volumeiosamples"}
	missedOnce := false
	c := newFakeClient(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
		obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*tierdv1.VolumeIOSample); ok && !missedOnce {
			missedOnce = true
			return apierrors.NewNotFound(sampleKind, key.Name)
		}
		return c.Get(ctx, key, obj, opts...)
	}}, boundPV(), tierState("fast"), sampleOwnedBy(testPVCUID))
	col := newPublishCollector(t, c)

	col.publish(context.Background())

	if got := getSample(t, c).Status.IOPS; got != 250 {
		t.Errorf("IOPS = %d, want 250", got)
	}
}

func TestWriteSampleErrors(t *testing.T) {
	injected := errors.New("injected")
	tests := []struct {
		name  string
		funcs interceptor.Funcs
	}{
		{"get", failGet[*tierdv1.VolumeIOSample]()},
		{"create", interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object,
			...client.CreateOption) error {
			return injected
		}}},
		{"patch status", interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string,
			client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return injected
		}}},
	}
	v := volume{pvName: testPV, kind: kindBlock, mount: procfs.Mount{Device: testDevice}}
	cl := claim{namespace: testNS, name: testPVC, uid: testPVCUID}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			col := newPublishCollector(t, newFakeClient(tt.funcs, boundPV(), tierState("fast")))
			if err := col.writeSample(context.Background(), cl, v, VolumeMetrics{}, nil); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestStartStopsWithContext(t *testing.T) {
	proc := newFakeProc(t, "")
	col := &Collector{
		Client: newFakeClient(interceptor.Funcs{}),
		Config: Config{ProcRoot: proc.root, SampleInterval: time.Millisecond, PublishInterval: time.Millisecond},
	}
	col.Reader = col.Client
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- col.Start(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
}
