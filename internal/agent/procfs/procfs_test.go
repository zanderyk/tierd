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
	"os"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestParseMountInfo(t *testing.T) {
	mounts, err := ParseMountInfo(open(t, "mountinfo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 6 {
		t.Fatalf("got %d mounts, want 6", len(mounts))
	}

	csi := mounts[3]
	if csi.Device != "8:32" || csi.FSType != "ext4" || csi.Source != "/dev/sdc" {
		t.Errorf("CSI mount = %+v", csi)
	}
	uid, pv, ok := ParseCSIMountPoint(csi.MountPoint)
	if !ok || uid != "3b7e2a10-6c1d-4a7e-9f0a-1d2c3b4a5e6f" || pv != "pvc-0a1b2c3d" {
		t.Errorf("ParseCSIMountPoint = %q, %q, %v", uid, pv, ok)
	}

	for _, i := range []int{0, 2, 4} {
		if _, _, ok := ParseCSIMountPoint(mounts[i].MountPoint); ok {
			t.Errorf("mount %d (%s) parsed as a CSI volume mount", i, mounts[i].MountPoint)
		}
	}

	nfs := mounts[5]
	if nfs.MountPoint != "/mnt/with space" || nfs.Source != "10.0.0.4:/export one" {
		t.Errorf("octal escapes not decoded: %+v", nfs)
	}
}

func TestParseMountInfoMalformed(t *testing.T) {
	if _, err := ParseMountInfo(strings.NewReader("22 1 8:1 / / rw\n")); err == nil {
		t.Error("want error for a line without the \" - \" separator")
	}
}

func TestParseDiskStats(t *testing.T) {
	stats, err := ParseDiskStats(open(t, "diskstats"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 4 {
		t.Fatalf("got %d devices, want 4", len(stats))
	}
	want := DiskStat{
		Name: "sdc", ReadsCompleted: 5000, SectorsRead: 400000, ReadTimeMs: 2500,
		WritesComplete: 15000, SectorsWritten: 1200000, WriteTimeMs: 9000,
		InFlight: 2, IOTimeMs: 8000, WeightedTimeMs: 11500,
	}
	if got := stats["8:32"]; got != want {
		t.Errorf("8:32 = %+v, want %+v", got, want)
	}
	if stats["259:0"].Name != "nvme0n1" {
		t.Errorf("14-field line not parsed: %+v", stats["259:0"])
	}
}

func TestDelta(t *testing.T) {
	prev := DiskStat{
		ReadsCompleted: 1000, WritesComplete: 1000, SectorsRead: 8000, SectorsWritten: 8000,
		ReadTimeMs: 1000, WriteTimeMs: 1000, IOTimeMs: 5000, WeightedTimeMs: 5000,
	}
	// Over 10s: 500 reads + 1500 writes, 20480 sectors (10 MiB), 4000 ms of
	// I/O time, busy 5000 ms, 20000 weighted ms.
	cur := DiskStat{
		ReadsCompleted: 1500, WritesComplete: 2500, SectorsRead: 8000 + 10240, SectorsWritten: 8000 + 10240,
		ReadTimeMs: 2000, WriteTimeMs: 4000, IOTimeMs: 10000, WeightedTimeMs: 25000,
	}
	iv, ok := Delta(prev, cur, 10*time.Second)
	if !ok {
		t.Fatal("Delta not ok")
	}
	if iv.IOPS != 200 {
		t.Errorf("IOPS = %v, want 200", iv.IOPS)
	}
	if iv.BytesPerSecond != 1024*1024 {
		t.Errorf("BytesPerSecond = %v, want 1 MiB", iv.BytesPerSecond)
	}
	if iv.AvgLatency != 2*time.Millisecond {
		t.Errorf("AvgLatency = %v, want 2ms", iv.AvgLatency)
	}
	if iv.Utilization != 0.5 {
		t.Errorf("Utilization = %v, want 0.5", iv.Utilization)
	}
	if iv.QueueDepth != 2 {
		t.Errorf("QueueDepth = %v, want 2", iv.QueueDepth)
	}
}

func TestDeltaRejectsResetAndZeroElapsed(t *testing.T) {
	prev := DiskStat{ReadsCompleted: 100}
	if _, ok := Delta(prev, DiskStat{ReadsCompleted: 5}, time.Second); ok {
		t.Error("want !ok when a counter goes backwards")
	}
	if _, ok := Delta(prev, prev, 0); ok {
		t.Error("want !ok for zero elapsed")
	}
	iv, ok := Delta(prev, prev, time.Second)
	if !ok || iv.AvgLatency != 0 || iv.IOPS != 0 {
		t.Errorf("idle interval = %+v, %v", iv, ok)
	}
}

func TestParsePressure(t *testing.T) {
	p, err := ParsePressure(open(t, "io.pressure"))
	if err != nil {
		t.Fatal(err)
	}
	want := Pressure{
		Some: PressureLine{Avg10: 12.5, Avg60: 34.25, Avg300: 8, TotalMicros: 123456789},
		Full: PressureLine{Avg10: 10, Avg60: 30, Avg300: 6.5, TotalMicros: 98765432},
	}
	if p != want {
		t.Errorf("got %+v, want %+v", p, want)
	}

	if _, err := ParsePressure(strings.NewReader("full avg10=1.00 avg60=1.00 avg300=1.00 total=1\n")); err == nil {
		t.Error("want error when the \"some\" line is missing")
	}
}
