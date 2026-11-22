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

package procfs

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

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
	iv, ok := Delta(prev.Counters(), cur.Counters(), 10*time.Second)
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
	reset := DiskStat{ReadsCompleted: 5}
	if _, ok := Delta(prev.Counters(), reset.Counters(), time.Second); ok {
		t.Error("want !ok when a counter goes backwards")
	}
	if _, ok := Delta(prev.Counters(), prev.Counters(), 0); ok {
		t.Error("want !ok for zero elapsed")
	}
	iv, ok := Delta(prev.Counters(), prev.Counters(), time.Second)
	if !ok || iv.AvgLatency != 0 || iv.IOPS != 0 {
		t.Errorf("idle interval = %+v, %v", iv, ok)
	}
}

func TestParseDiskStatsErrors(t *testing.T) {
	tests := []struct {
		name, in string
	}{
		{"too few fields", "8 0 sda 1 2 3 4 5 6 7 8 9 10\n"},
		{"non-numeric counter", "8 0 sda 1 2 x 4 5 6 7 8 9 10 11\n"},
		{"negative counter", "8 0 sda 1 2 3 4 5 6 7 8 9 10 -11\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseDiskStats(strings.NewReader(tt.in)); err == nil {
				t.Errorf("want error for %q", tt.in)
			}
		})
	}
}

func TestParseDiskStatsSkipsBlankLinesAndExtraFields(t *testing.T) {
	// Kernels 4.18+ append discard counters and 5.5+ flush counters; those
	// trailing fields must be ignored.
	in := "\n   \n" +
		"   8       0 sda 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20\n"
	stats, err := ParseDiskStats(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := DiskStat{
		Name: "sda", ReadsCompleted: 1, SectorsRead: 3, ReadTimeMs: 4,
		WritesComplete: 5, SectorsWritten: 7, WriteTimeMs: 8,
		InFlight: 9, IOTimeMs: 10, WeightedTimeMs: 11,
	}
	if len(stats) != 1 || stats["8:0"] != want {
		t.Errorf("got %+v, want only 8:0 = %+v", stats, want)
	}
}

func TestParseDiskStatsReadError(t *testing.T) {
	if _, err := ParseDiskStats(iotest.ErrReader(errors.New("boom"))); err == nil {
		t.Error("want error when the reader fails")
	}
}

func TestDeltaRejectsEachBackwardsCounter(t *testing.T) {
	prev := DiskStat{
		ReadsCompleted: 10, WritesComplete: 10, SectorsRead: 10, SectorsWritten: 10,
		ReadTimeMs: 10, WriteTimeMs: 10, IOTimeMs: 10, WeightedTimeMs: 10,
	}
	fields := map[string]func(*DiskStat){
		"ReadsCompleted": func(d *DiskStat) { d.ReadsCompleted = 9 },
		"WritesComplete": func(d *DiskStat) { d.WritesComplete = 9 },
		"SectorsRead":    func(d *DiskStat) { d.SectorsRead = 9 },
		"SectorsWritten": func(d *DiskStat) { d.SectorsWritten = 9 },
		"ReadTimeMs":     func(d *DiskStat) { d.ReadTimeMs = 9 },
		"WriteTimeMs":    func(d *DiskStat) { d.WriteTimeMs = 9 },
		"IOTimeMs":       func(d *DiskStat) { d.IOTimeMs = 9 },
		"WeightedTimeMs": func(d *DiskStat) { d.WeightedTimeMs = 9 },
	}
	for name, regress := range fields {
		t.Run(name, func(t *testing.T) {
			cur := prev
			regress(&cur)
			if _, ok := Delta(prev.Counters(), cur.Counters(), time.Second); ok {
				t.Errorf("want !ok when %s goes backwards", name)
			}
		})
	}
	if _, ok := Delta(prev.Counters(), prev.Counters(), -time.Second); ok {
		t.Error("want !ok for negative elapsed")
	}
}

func TestDeltaClampsUtilization(t *testing.T) {
	// io_ticks can advance slightly more than wall time between two reads.
	prev := DiskStat{IOTimeMs: 0}
	cur := DiskStat{IOTimeMs: 1500}
	iv, ok := Delta(prev.Counters(), cur.Counters(), time.Second)
	if !ok {
		t.Fatal("Delta not ok")
	}
	if iv.Utilization != 1 {
		t.Errorf("Utilization = %v, want clamped to 1", iv.Utilization)
	}
	if iv.Elapsed != time.Second {
		t.Errorf("Elapsed = %v, want 1s", iv.Elapsed)
	}
}
