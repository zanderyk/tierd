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

package procfs

import (
	"strings"
	"testing"
	"time"
)

func TestParseMountStats(t *testing.T) {
	stats, err := ParseMountStats(open(t, "mountstats"))
	if err != nil {
		t.Fatal(err)
	}
	// Two Azure Files mounts (globalmount + pod) and one with escapes; the
	// ext4, sysfs and proc mounts are skipped.
	if len(stats) != 3 {
		t.Fatalf("got %d mounts, want 3: %+v", len(stats), stats)
	}

	// Captured on AKS: Azure Files premium NFS with dd writing 1 MiB blocks.
	pod := "/var/lib/kubelet/pods/15e3d643-8074-4255-9630-f6f9fd48e063/volumes/kubernetes.io~csi/pvc-e7e4eeae-e5f9-42b8-bd1d-4235b71af01c/mount"
	want := Counters{
		Writes: 3750, BytesWritten: 524288000,
		WriteMs: 72977, WeightedMs: 72977,
		HasLatency: true,
	}
	if got := stats[pod]; got != want {
		t.Errorf("pod mount = %+v, want %+v", got, want)
	}

	// Older kernels have 8 per-op fields (no errors); paths are escaped.
	want = Counters{Reads: 2, BytesRead: 2097152, ReadMs: 20, WeightedMs: 20, HasLatency: true}
	if got := stats["/mnt/with space"]; got != want {
		t.Errorf("/mnt/with space = %+v, want %+v", got, want)
	}
}

func TestParseMountStatsErrors(t *testing.T) {
	const dev = "device h:/e mounted on /m with fstype nfs4 statvers=1.1\n"
	tests := []struct {
		name, in string
	}{
		{"short bytes line", dev + "\tbytes:\t1 2 3\n"},
		{"non-numeric bytes", dev + "\tbytes:\t0 0 0 0 x 0 0 0\n"},
		{"short op line", dev + "\t\tWRITE: 1 2 3\n"},
		{"non-numeric ops", dev + "\t\tWRITE: x 0 0 0 0 0 0 0 0\n"},
		{"non-numeric execute", dev + "\t\tREAD: 1 1 0 0 0 0 0 x 0\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseMountStats(strings.NewReader(tt.in)); err == nil {
				t.Errorf("want error for %q", tt.in)
			}
		})
	}
}

func TestDeltaLatencyWithoutUtilization(t *testing.T) {
	// NFS: 1000 writes taking 20 s of execute time over 10 s.
	prev := Counters{HasLatency: true}
	cur := Counters{Writes: 1000, BytesWritten: 1_000_000_000, WriteMs: 20_000, WeightedMs: 20_000, HasLatency: true}
	iv, ok := Delta(prev, cur, 10*time.Second)
	if !ok {
		t.Fatal("Delta not ok")
	}
	if iv.IOPS != 100 || iv.AvgLatency != 20*time.Millisecond || iv.QueueDepth != 2 {
		t.Errorf("interval = %+v, want 100 IOPS, 20ms, queue depth 2", iv)
	}
	if !iv.HasLatency || iv.HasUtilization || iv.Utilization != 0 {
		t.Errorf("want latency without utilization: %+v", iv)
	}
}
