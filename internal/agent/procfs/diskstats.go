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
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// sectorSize is fixed at 512 bytes in diskstats regardless of the device's
// logical block size.
const sectorSize = 512

// DiskStat is the cumulative counters of one /proc/diskstats line. See
// Documentation/admin-guide/iostats.rst.
type DiskStat struct {
	Name           string
	ReadsCompleted uint64
	SectorsRead    uint64
	ReadTimeMs     uint64
	WritesComplete uint64
	SectorsWritten uint64
	WriteTimeMs    uint64
	InFlight       uint64
	IOTimeMs       uint64
	WeightedTimeMs uint64
}

// Counters is cumulative I/O counters for one volume from any source.
type Counters struct {
	Reads, Writes           uint64
	BytesRead, BytesWritten uint64
	// ReadMs, WriteMs and WeightedMs are time spent on I/O. Set when
	// HasLatency (block, nfs).
	ReadMs, WriteMs, WeightedMs uint64
	// IOTicksMs is time with I/O in flight. Set when HasUtilization (block).
	IOTicksMs      uint64
	HasLatency     bool
	HasUtilization bool
}

func (d *DiskStat) Counters() Counters {
	return Counters{
		Reads:          d.ReadsCompleted,
		Writes:         d.WritesComplete,
		BytesRead:      d.SectorsRead * sectorSize,
		BytesWritten:   d.SectorsWritten * sectorSize,
		ReadMs:         d.ReadTimeMs,
		WriteMs:        d.WriteTimeMs,
		IOTicksMs:      d.IOTimeMs,
		WeightedMs:     d.WeightedTimeMs,
		HasLatency:     true,
		HasUtilization: true,
	}
}

// ParseDiskStats parses /proc/diskstats into counters keyed by "major:minor".
func ParseDiskStats(r io.Reader) (map[string]DiskStat, error) {
	stats := make(map[string]DiskStat)
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) < 14 {
			return nil, fmt.Errorf("diskstats line %d: want at least 14 fields, got %d", line, len(f))
		}
		var v [11]uint64
		for i := range v {
			n, err := strconv.ParseUint(f[i+3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("diskstats line %d field %d: %w", line, i+4, err)
			}
			v[i] = n
		}
		stats[f[0]+":"+f[1]] = DiskStat{
			Name:           f[2],
			ReadsCompleted: v[0],
			SectorsRead:    v[2],
			ReadTimeMs:     v[3],
			WritesComplete: v[4],
			SectorsWritten: v[6],
			WriteTimeMs:    v[7],
			InFlight:       v[8],
			IOTimeMs:       v[9],
			WeightedTimeMs: v[10],
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cannot read diskstats: %w", err)
	}
	return stats, nil
}

// Interval is the device activity between two Counters readings.
type Interval struct {
	Elapsed time.Duration
	// IOPS is completed reads plus writes per second.
	IOPS float64
	// BytesPerSecond is bytes read plus written per second.
	BytesPerSecond float64
	// AvgLatency is the mean time per completed I/O; zero when idle.
	AvgLatency time.Duration
	// Utilization is the fraction (0..1) of the interval with I/O in flight.
	Utilization float64
	// QueueDepth is the average number of requests in flight.
	QueueDepth float64
	// HasLatency is false when the source has no timing counters (cifs):
	// AvgLatency and QueueDepth are then unset.
	HasLatency bool
	// HasUtilization is false when the source has no busy time (cifs, nfs):
	// Utilization is then unset.
	HasUtilization bool
}

// Delta computes the activity between prev and cur taken elapsed apart.
// ok is false if elapsed is not positive or a counter went backwards (the
// device was replaced or the counters reset), in which case the interval
// should be skipped.
func Delta(prev, cur Counters, elapsed time.Duration) (Interval, bool) {
	if elapsed <= 0 {
		return Interval{}, false
	}
	pairs := [][2]uint64{
		{prev.Reads, cur.Reads},
		{prev.Writes, cur.Writes},
		{prev.BytesRead, cur.BytesRead},
		{prev.BytesWritten, cur.BytesWritten},
		{prev.ReadMs, cur.ReadMs},
		{prev.WriteMs, cur.WriteMs},
		{prev.IOTicksMs, cur.IOTicksMs},
		{prev.WeightedMs, cur.WeightedMs},
	}
	for _, p := range pairs {
		if p[1] < p[0] {
			return Interval{}, false
		}
	}

	ios := float64((cur.Reads - prev.Reads) + (cur.Writes - prev.Writes))
	bytes := float64((cur.BytesRead - prev.BytesRead) + (cur.BytesWritten - prev.BytesWritten))
	secs := elapsed.Seconds()

	iv := Interval{
		Elapsed:        elapsed,
		IOPS:           ios / secs,
		BytesPerSecond: bytes / secs,
		HasLatency:     cur.HasLatency,
		HasUtilization: cur.HasUtilization,
	}

	elapsedMs := float64(elapsed.Milliseconds())
	if cur.HasLatency {
		ioMs := float64((cur.ReadMs - prev.ReadMs) + (cur.WriteMs - prev.WriteMs))
		iv.QueueDepth = float64(cur.WeightedMs-prev.WeightedMs) / elapsedMs
		if ios > 0 {
			iv.AvgLatency = time.Duration(ioMs / ios * float64(time.Millisecond))
		}
	}
	if cur.HasUtilization {
		iv.Utilization = min(float64(cur.IOTicksMs-prev.IOTicksMs)/elapsedMs, 1)
	}
	return iv, true
}
