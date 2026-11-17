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
	"testing"
	"time"

	"github.com/zanderyk/tierd/internal/agent/procfs"
)

func TestWindowSummarize(t *testing.T) {
	w := NewWindow(time.Minute)
	if _, ok := w.Summarize(); ok {
		t.Fatal("empty window should not summarize")
	}

	// Twenty 10s intervals with latencies 1ms..20ms; only the last six
	// (15..20ms) fit in a one-minute window.
	for i := 1; i <= 20; i++ {
		w.Add(procfs.Interval{
			Elapsed:        10 * time.Second,
			IOPS:           100,
			BytesPerSecond: 1000,
			AvgLatency:     time.Duration(i) * time.Millisecond,
			Utilization:    0.5,
			QueueDepth:     1,
		})
	}

	s, ok := w.Summarize()
	if !ok {
		t.Fatal("Summarize not ok")
	}
	if s.Samples != 6 || s.Span != time.Minute {
		t.Errorf("window holds %d samples over %v, want 6 over 1m", s.Samples, s.Span)
	}
	if s.IOPS != 100 || s.BytesPerSecond != 1000 || s.Utilization != 0.5 || s.QueueDepth != 1 {
		t.Errorf("rates = %+v", s)
	}
	if s.LatencyP50 != 17*time.Millisecond {
		t.Errorf("P50 = %v, want 17ms", s.LatencyP50)
	}
	if s.LatencyP95 != 20*time.Millisecond {
		t.Errorf("P95 = %v, want 20ms", s.LatencyP95)
	}
}

func TestWindowIgnoresIdleIntervalsForLatency(t *testing.T) {
	w := NewWindow(time.Minute)
	w.Add(procfs.Interval{Elapsed: 30 * time.Second})
	w.Add(procfs.Interval{Elapsed: 30 * time.Second, IOPS: 10, AvgLatency: 5 * time.Millisecond})

	s, _ := w.Summarize()
	if s.LatencyP50 != 5*time.Millisecond || s.LatencyP95 != 5*time.Millisecond {
		t.Errorf("idle interval affected latency: %+v", s)
	}
	if s.IOPS != 5 {
		t.Errorf("IOPS = %v, want time-weighted 5", s.IOPS)
	}
}
