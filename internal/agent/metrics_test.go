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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// seriesCount is the number of series across all volume gauges.
func seriesCount(t *testing.T) int {
	t.Helper()
	n := 0
	for _, g := range volumeGauges {
		n += testutil.CollectAndCount(g)
	}
	return n
}

func TestVolumeMetrics(t *testing.T) {
	cl := claim{namespace: "default", name: "data"}
	m := VolumeMetrics{IOPS: 3000, LatencyP95: 20 * time.Millisecond, Utilization: 0.5, HasLatency: true, HasUtilization: true}
	psi := 25.0

	SetVolumeMetrics(cl, "pvc-a", "node-1", m, &psi)
	if got := seriesCount(t); got != len(volumeGauges) {
		t.Fatalf("after set: %d series, want %d", got, len(volumeGauges))
	}
	if got := testutil.ToFloat64(volumeLatencyP95); got != 0.02 {
		t.Errorf("latency = %v, want 0.02 seconds", got)
	}
	if got := testutil.ToFloat64(volumeIOPressure); got != 0.25 {
		t.Errorf("io pressure = %v, want 0.25", got)
	}

	SetVolumeMetrics(cl, "pvc-a", "node-1", m, nil)
	if got := testutil.CollectAndCount(volumeIOPressure); got != 0 {
		t.Errorf("unknown pressure: %d io pressure series, want 0", got)
	}

	DeleteVolumeMetrics("pvc-a")
	if got := seriesCount(t); got != 0 {
		t.Errorf("after delete: %d series, want 0", got)
	}
}

func TestSampleDeletesMetricsOfUnmountedVolume(t *testing.T) {
	// A host /proc with no volumes mounted.
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join("1", "mountinfo"), "diskstats"} {
		if err := os.WriteFile(filepath.Join(proc, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cl := claim{namespace: "default", name: "data"}
	c := &Collector{
		Config: Config{ProcRoot: proc},
		claims: map[string]claim{"pvc-a": cl},
	}
	SetVolumeMetrics(cl, "pvc-a", "node-1", VolumeMetrics{IOPS: 100}, nil)

	if err := c.sample(time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := seriesCount(t); got != 0 {
		t.Errorf("after unmount: %d series, want 0", got)
	}
}
