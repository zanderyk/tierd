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

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zanderyk/tierd/internal/agent/procfs"
)

func TestDiscoverVolumes(t *testing.T) {
	const (
		kubelet = "/var/lib/kubelet/pods/"
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
	}

	vols := discoverVolumes(mounts)
	if len(vols) != 4 {
		t.Fatalf("got %d volumes, want 4 (pvc-a, pvc-b, pvc-nfs, pvc-smb): %+v", len(vols), vols)
	}
	a := vols["pvc-a"]
	if a.mount.Device != sdc || !slices.Equal(a.podUIDs, []string{"aaaa-1", "bbbb-2"}) {
		t.Errorf("pvc-a = %+v", a)
	}
	if vols["pvc-b"].mount.Device != "8:48" {
		t.Errorf("pvc-b = %+v", vols["pvc-b"])
	}
	for pv, want := range map[string]kind{"pvc-a": kindBlock, "pvc-smb": kindCifs, "pvc-nfs": kindNone} {
		if got := vols[pv].kind; got != want {
			t.Errorf("%s kind = %q, want %q", pv, got, want)
		}
	}
}

func TestPodCgroupPath(t *testing.T) {
	root := t.TempDir()
	burstable := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
		"kubepods-burstable-pod3b7e2a10_6c1d_4a7e_9f0a_1d2c3b4a5e6f.slice")
	guaranteed := filepath.Join(root, "kubepods", "pod11111111-2222-3333-4444-555555555555")
	for _, dir := range []string{burstable, guaranteed} {
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
	if _, err := podCgroupPath(root, "missing"); err == nil {
		t.Error("want error for unknown pod")
	}
}

func TestSampleCifsVolume(t *testing.T) {
	const share = `\\acct.file.core.windows.net\pvc-c`
	proc := t.TempDir()
	write := func(name, data string) {
		t.Helper()
		p := filepath.Join(proc, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stats := func(writes, bytes int) string {
		return fmt.Sprintf("1) %s\nBytes read: 0  Bytes written: %d\nReads: 0 total 0 failed\nWrites: %d total 0 failed\n",
			share, bytes, writes)
	}
	write("1/mountinfo", "600 22 0:297 / /var/lib/kubelet/pods/aaaa-1/volumes/kubernetes.io~csi/pvc-c/mount "+
		"rw shared:400 - cifs //acct.file.core.windows.net/pvc-c rw\n")
	write("diskstats", "")
	write("fs/cifs/Stats", stats(1000, 1_000_000))

	c := &Collector{
		Config:  Config{ProcRoot: proc, Window: time.Minute},
		prev:    map[string]reading{},
		windows: map[string]*Window{},
	}
	start := time.Now()
	if err := c.sample(start); err != nil {
		t.Fatal(err)
	}
	write("fs/cifs/Stats", stats(2000, 11_000_000))
	if err := c.sample(start.Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	w := c.windows["0:297"]
	if w == nil {
		t.Fatal("no window for the cifs volume")
	}
	s, ok := w.Summarize()
	if !ok {
		t.Fatal("Summarize not ok")
	}
	if s.IOPS != 100 || s.BytesPerSecond != 1_000_000 || s.HasTiming {
		t.Errorf("summary = %+v, want 100 IOPS, 1 MB/s, no timing", s)
	}
}
