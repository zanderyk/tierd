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
	"math"
	"slices"
	"time"

	"github.com/zanderyk/tierd/internal/agent/procfs"
)

// Window keeps the device intervals covering roughly the last span.
type Window struct {
	span      time.Duration
	intervals []procfs.Interval
}

// NewWindow returns an empty window covering span.
func NewWindow(span time.Duration) *Window {
	return &Window{span: span}
}

// Add appends an interval and drops the oldest ones once the window holds
// more than span.
func (w *Window) Add(iv procfs.Interval) {
	w.intervals = append(w.intervals, iv)
	var total time.Duration
	for _, x := range w.intervals {
		total += x.Elapsed
	}
	for len(w.intervals) > 1 && total-w.intervals[0].Elapsed >= w.span {
		total -= w.intervals[0].Elapsed
		w.intervals = w.intervals[1:]
	}
}

// VolumeMetrics is a window's intervals reduced to the figures in a VolumeIOSample.
type VolumeMetrics struct {
	Span           time.Duration
	Samples        int
	IOPS           float64
	BytesPerSecond float64
	// LatencyP50 and LatencyP95 are percentiles of the per-interval average
	// latency, over intervals that completed at least one I/O.
	LatencyP50  time.Duration
	LatencyP95  time.Duration
	Utilization float64
	QueueDepth  float64
	// HasLatency is false when the source has no timing counters (cifs):
	// LatencyP50, LatencyP95 and QueueDepth are unset.
	HasLatency bool
	// HasUtilization is false when the source has no busy time (cifs, nfs):
	// Utilization is unset.
	HasUtilization bool
}

// Summarize reduces the window. Rates are time-weighted means; ok is false
// if the window is empty.
func (w *Window) Summarize() (VolumeMetrics, bool) {
	if len(w.intervals) == 0 {
		return VolumeMetrics{}, false
	}
	// A volume's intervals all come from one source.
	s := VolumeMetrics{
		Samples:        len(w.intervals),
		HasLatency:     w.intervals[0].HasLatency,
		HasUtilization: w.intervals[0].HasUtilization,
	}
	var lat []time.Duration
	for _, iv := range w.intervals {
		secs := iv.Elapsed.Seconds()
		s.Span += iv.Elapsed
		s.IOPS += iv.IOPS * secs
		s.BytesPerSecond += iv.BytesPerSecond * secs
		s.Utilization += iv.Utilization * secs
		s.QueueDepth += iv.QueueDepth * secs
		if iv.IOPS > 0 {
			lat = append(lat, iv.AvgLatency)
		}
	}
	total := s.Span.Seconds()
	s.IOPS /= total
	s.BytesPerSecond /= total
	s.Utilization /= total
	s.QueueDepth /= total
	if s.HasLatency {
		slices.Sort(lat)
		s.LatencyP50 = percentile(lat, 50)
		s.LatencyP95 = percentile(lat, 95)
	}
	return s, true
}

// percentile is the nearest-rank percentile of sorted; zero if empty.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}
