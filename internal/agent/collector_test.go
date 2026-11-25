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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	tierdv1alpha1 "github.com/zanderyk/tierd/api/v1alpha1"
	"github.com/zanderyk/tierd/internal/agent/procfs"
)

func TestDiscoverVolumes(t *testing.T) {
	const (
		kubelet = "/var/lib/kubelet/pods/"
		plugins = "/var/lib/kubelet/plugins/kubernetes.io/csi/volumeDevices/publish/"
		sdc     = "8:32"
	)
	mounts := []procfs.Mount{
		{Device: "8:1", MountPoint: "/"},
		{Device: sdc, MountPoint: kubelet + "aaaa-1/volumes/kubernetes.io~csi/pvc-a/mount"},
		{Device: sdc, MountPoint: kubelet + "bbbb-2/volumes/kubernetes.io~csi/pvc-a/mount"},
		{Device: "8:48", MountPoint: kubelet + "aaaa-1/volumes/kubernetes.io~csi/pvc-b/mount"},
		{Device: "0:88", FSType: "nfs4", MountPoint: kubelet + "cccc-3/volumes/kubernetes.io~csi/pvc-nfs/mount"},
		{Device: "0:97", FSType: "cifs", MountPoint: kubelet + "cccc-3/volumes/kubernetes.io~csi/pvc-smb/mount"},
		{Device: "0:61", MountPoint: kubelet + "aaaa-1/volumes/kubernetes.io~projected/token"},
		// Block-mode: a bind mount of /dev/nvme0n4, so the mount's device is
		// devtmpfs's; the disk comes from diskstats by name.
		{Device: "0:5", Root: "/nvme0n4", FSType: "devtmpfs", MountPoint: plugins + "pvc-raw/dddd-4"},
		// Block-mode on a device diskstats doesn't list: skipped.
		{Device: "0:5", Root: "/mapper/x", FSType: "devtmpfs", MountPoint: plugins + "pvc-dm/dddd-4"},
	}
	disk := map[string]procfs.DiskStat{"259:8": {Name: "nvme0n4"}}

	vols := discoverVolumes(mounts, disk)
	if len(vols) != 5 {
		t.Fatalf("got %d volumes, want 5 (pvc-a, pvc-b, pvc-nfs, pvc-smb, pvc-raw): %+v", len(vols), vols)
	}
	raw := vols["pvc-raw"]
	if raw.mount.Device != "259:8" || raw.kind != kindBlock || !slices.Equal(raw.podUIDs, []string{"dddd-4"}) {
		t.Errorf("pvc-raw = %+v", raw)
	}
	a := vols["pvc-a"]
	if a.mount.Device != sdc || !slices.Equal(a.podUIDs, []string{"aaaa-1", "bbbb-2"}) {
		t.Errorf("pvc-a = %+v", a)
	}
	if vols["pvc-b"].mount.Device != "8:48" {
		t.Errorf("pvc-b = %+v", vols["pvc-b"])
	}
	for pv, want := range map[string]kind{"pvc-a": kindBlock, "pvc-smb": kindCifs, "pvc-nfs": kindNFS} {
		if got := vols[pv].kind; got != want {
			t.Errorf("%s kind = %q, want %q", pv, got, want)
		}
	}
}

func TestCifsShare(t *testing.T) {
	stats := map[string]procfs.Counters{"//srv/share": {Reads: 7}}
	for _, tc := range []struct {
		source string
		ok     bool
	}{
		{"//srv/share", true},
		{"//srv/share/pvc-1", true},
		{"//srv/share/a/b", true},
		{"//srv/other", false},
		{"//srv/sharex", false},
		{"//srv", false},
		{"share", false},
	} {
		c, ok := cifsShare(stats, tc.source)
		if ok != tc.ok || (ok && c.Reads != 7) {
			t.Errorf("cifsShare(%q) = %+v, %v; want ok %v", tc.source, c, ok, tc.ok)
		}
	}
}

func TestPodCgroupPath(t *testing.T) {
	root := t.TempDir()
	burstable := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
		"kubepods-burstable-pod3b7e2a10_6c1d_4a7e_9f0a_1d2c3b4a5e6f.slice")
	guaranteed := filepath.Join(root, "kubepods", "pod11111111-2222-3333-4444-555555555555")
	// kind runs the kubelet with --cgroup-root=/kubelet.
	kindPod := filepath.Join(root, "kubelet.slice", "kubelet-kubepods.slice", "kubelet-kubepods-besteffort.slice",
		"kubelet-kubepods-besteffort-pod22222222_3333_4444_5555_666666666666.slice")
	for _, dir := range []string{burstable, guaranteed, kindPod, filepath.Join(root, "system.slice", "pod-lookalike")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := podCgroupPath(root, "3b7e2a10-6c1d-4a7e-9f0a-1d2c3b4a5e6f")
	if err != nil || got != burstable {
		t.Errorf("systemd burstable: got %q, %v", got, err)
	}
	got, err = podCgroupPath(root, "11111111-2222-3333-4444-555555555555")
	if err != nil || got != guaranteed {
		t.Errorf("cgroupfs guaranteed: got %q, %v", got, err)
	}
	got, err = podCgroupPath(root, "22222222-3333-4444-5555-666666666666")
	if err != nil || got != kindPod {
		t.Errorf("kubelet cgroup root: got %q, %v", got, err)
	}
	if _, err := podCgroupPath(root, "missing"); err == nil {
		t.Error("want error for unknown pod")
	}
}

// fakeProc is a host /proc in a temp dir for driving sample().
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T, mountinfo string) fakeProc {
	p := fakeProc{t: t, root: t.TempDir()}
	p.write("1/mountinfo", mountinfo)
	p.write("diskstats", "")
	return p
}

func (p fakeProc) write(name, data string) {
	p.t.Helper()
	path := filepath.Join(p.root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		p.t.Fatal(err)
	}
}

// sampleTwice runs sample() 10s apart, calling between() in the middle, and
// returns the summary of the volume on device.
func sampleTwice(t *testing.T, p fakeProc, device string, between func()) VolumeMetrics {
	t.Helper()
	c := &Collector{
		Config:  Config{ProcRoot: p.root, Window: time.Minute},
		prev:    map[string]reading{},
		windows: map[string]*Window{},
	}
	start := time.Now()
	if err := c.sample(start); err != nil {
		t.Fatal(err)
	}
	between()
	if err := c.sample(start.Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	w := c.windows[device]
	if w == nil {
		t.Fatalf("no window for device %s", device)
	}
	s, ok := w.Summarize()
	if !ok {
		t.Fatal("Summarize not ok")
	}
	return s
}

func TestSampleCifsVolume(t *testing.T) {
	stats := func(writes, bytes int) string {
		return fmt.Sprintf("1) \\\\acct.file.core.windows.net\\pvc-c\nBytes read: 0  Bytes written: %d\n"+
			"Reads: 0 total 0 failed\nWrites: %d total 0 failed\n", bytes, writes)
	}
	p := newFakeProc(t, "600 22 0:297 / /var/lib/kubelet/pods/aaaa-1/volumes/kubernetes.io~csi/pvc-c/mount "+
		"rw shared:400 - cifs //acct.file.core.windows.net/pvc-c rw\n")
	p.write("fs/cifs/Stats", stats(1000, 1_000_000))

	s := sampleTwice(t, p, "0:297", func() { p.write("fs/cifs/Stats", stats(2000, 11_000_000)) })
	if s.IOPS != 100 || s.BytesPerSecond != 1_000_000 || s.HasLatency || s.HasUtilization {
		t.Errorf("summary = %+v, want 100 IOPS, 1 MB/s, no timing", s)
	}
}

func TestSampleNFSVolume(t *testing.T) {
	const mnt = "/var/lib/kubelet/pods/aaaa-1/volumes/kubernetes.io~csi/pvc-n/mount"
	stats := func(writes, bytes, executeMs int) string {
		return fmt.Sprintf("device acct:/acct/pvcn-n mounted on %s with fstype nfs4 statvers=1.1\n"+
			"\tbytes:\t0 0 0 0 0 %d 0 0\n\tper-op statistics\n"+
			"\t        READ: 0 0 0 0 0 0 0 0 0\n\t       WRITE: %d %d 0 0 0 0 0 %d 0\n",
			mnt, bytes, writes, writes, executeMs)
	}
	p := newFakeProc(t, "700 22 0:583 / "+mnt+" rw shared:500 - nfs4 acct:/acct/pvcn-n rw\n")
	p.write("1/mountstats", stats(1000, 1_000_000, 10_000))

	// 1000 writes and 20 s of execute time in 10 s.
	s := sampleTwice(t, p, "0:583", func() { p.write("1/mountstats", stats(2000, 11_000_000, 30_000)) })
	if s.IOPS != 100 || s.BytesPerSecond != 1_000_000 || s.LatencyP95 != 20*time.Millisecond || s.QueueDepth != 2 {
		t.Errorf("summary = %+v, want 100 IOPS, 1 MB/s, 20ms, queue depth 2", s)
	}
	if !s.HasLatency || s.HasUtilization {
		t.Errorf("want latency without utilization: %+v", s)
	}
}

func TestMeasured(t *testing.T) {
	block := VolumeMetrics{HasLatency: true, HasUtilization: true}
	nfs := VolumeMetrics{HasLatency: true}
	tests := []struct {
		name    string
		v       volume
		s       VolumeMetrics
		status  metav1.ConditionStatus
		reason  string
		message string
	}{
		{"block", volume{kind: kindBlock}, block, metav1.ConditionTrue, tierdv1alpha1.MeasuredComplete, ""},
		{"nfs", volume{kind: kindNFS}, nfs, metav1.ConditionTrue, tierdv1alpha1.MeasuredPartial,
			"nfs volumes report no utilization"},
		{"cifs", volume{kind: kindCifs}, VolumeMetrics{}, metav1.ConditionTrue, tierdv1alpha1.MeasuredPartial,
			"cifs volumes report no latency, queue depth, utilization"},
		{"blobfuse", volume{kind: kindNone, mount: procfs.Mount{FSType: "fuse.blobfuse2"}}, VolumeMetrics{},
			metav1.ConditionFalse, tierdv1alpha1.MeasuredNoSource, "tierd cannot measure fuse.blobfuse2 volumes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := measured(tt.v, tt.s)
			if got.Type != tierdv1alpha1.VolumeIOSampleMeasured || got.Status != tt.status ||
				got.Reason != tt.reason || got.Message != tt.message {
				t.Errorf("measured = %+v, want %s/%s %q", got, tt.status, tt.reason, tt.message)
			}
		})
	}
}
