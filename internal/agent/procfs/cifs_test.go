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

func TestParseCifsStats(t *testing.T) {
	stats, err := ParseCifsStats(open(t, "cifs-stats"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("got %d shares, want 2: %+v", len(stats), stats)
	}

	// Captured on AKS with dd writing to an Azure Files share.
	azure := "//fdd6e27d595f945cab6f708.file.core.windows.net/pvc-217448e1-51e1-46f7-8fbe-54b582e5f0ef"
	want := Counters{Writes: 4956, BytesWritten: 4875878400}
	if got := stats[azure]; got != want {
		t.Errorf("%s = %+v, want %+v", azure, got, want)
	}

	want = Counters{Reads: 16, BytesRead: 1048576}
	if got := stats["//10.0.0.5/data"]; got != want {
		t.Errorf("//10.0.0.5/data = %+v, want %+v", got, want)
	}
}

func TestParseCifsStatsNoShares(t *testing.T) {
	// cifs module loaded but nothing mounted.
	stats, err := ParseCifsStats(strings.NewReader("Resources in use\nCIFS Session: 0\n"))
	if err != nil || len(stats) != 0 {
		t.Errorf("got %+v, %v; want empty", stats, err)
	}
}

func TestParseCifsStatsErrors(t *testing.T) {
	tests := []struct {
		name, in string
	}{
		{"short bytes line", "1) \\\\h\\s\nBytes read: 5\n"},
		{"non-numeric bytes", "1) \\\\h\\s\nBytes read: x  Bytes written: 0\n"},
		{"non-numeric writes", "1) \\\\h\\s\nWrites: x total 0 failed\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseCifsStats(strings.NewReader(tt.in)); err == nil {
				t.Errorf("want error for %q", tt.in)
			}
		})
	}
}

func TestDeltaWithoutTiming(t *testing.T) {
	prev := Counters{Writes: 1000, BytesWritten: 1_000_000_000}
	cur := Counters{Writes: 2000, BytesWritten: 2_000_000_000}
	iv, ok := Delta(prev, cur, 10*time.Second)
	if !ok {
		t.Fatal("Delta not ok")
	}
	if iv.HasLatency || iv.HasUtilization {
		t.Errorf("want no timing flags: %+v", iv)
	}
	if iv.IOPS != 100 || iv.BytesPerSecond != 100_000_000 {
		t.Errorf("rates = %+v", iv)
	}
	if iv.AvgLatency != 0 || iv.Utilization != 0 || iv.QueueDepth != 0 {
		t.Errorf("timing figures set without timing: %+v", iv)
	}
}
