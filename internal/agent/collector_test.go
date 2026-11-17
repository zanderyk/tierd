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
	"os"
	"path/filepath"
	"slices"
	"testing"

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
		{Device: "0:88", MountPoint: kubelet + "cccc-3/volumes/kubernetes.io~csi/pvc-nfs/mount"},
		{Device: "0:61", MountPoint: kubelet + "aaaa-1/volumes/kubernetes.io~projected/token"},
	}

	vols := discoverVolumes(mounts)
	if len(vols) != 2 {
		t.Fatalf("got %d volumes, want 2 (pvc-a, pvc-b): %+v", len(vols), vols)
	}
	a := vols["pvc-a"]
	if a.device != sdc || !slices.Equal(a.podUIDs, []string{"aaaa-1", "bbbb-2"}) {
		t.Errorf("pvc-a = %+v", a)
	}
	if vols["pvc-b"].device != "8:48" {
		t.Errorf("pvc-b = %+v", vols["pvc-b"])
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
