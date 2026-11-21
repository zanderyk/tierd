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
)

// ParseMountStats parses the NFS mounts in /proc/<pid>/mountstats into
// counters keyed by mount point. Other filesystems are skipped.
//
//	device host:/export mounted on /path with fstype nfs4 statvers=1.1
//		bytes:	<normal r/w> <direct r/w> <server read> <server written> ...
//		per-op statistics
//		        READ: ops trans timeouts sent recv queue_ms rtt_ms execute_ms [errors]
//		       WRITE: ...
//
// Bytes are what went over the wire (server read/written). Latency is
// execute time (queue + RTT), which is what the application waits for.
// NFS has no busy-time counter, so HasUtilization is false.
func ParseMountStats(r io.Reader) (map[string]Counters, error) {
	stats := make(map[string]Counters)
	var mountPoint string // "" outside an NFS mount's block
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if f[0] == "device" {
			mountPoint = ""
			// device <src> mounted on <path> with fstype <type> ...
			if len(f) >= 8 && f[2] == "mounted" && f[3] == "on" && f[5] == "with" &&
				(f[7] == "nfs" || f[7] == "nfs4") {
				mountPoint = unescapeOctal(f[4])
				stats[mountPoint] = Counters{HasLatency: true}
			}
			continue
		}
		if mountPoint == "" {
			continue
		}

		c := stats[mountPoint]
		var err error
		switch f[0] {
		case "bytes:":
			if len(f) < 7 {
				return nil, fmt.Errorf("mountstats line %d: want 6 byte counters, got %d", line, len(f)-1)
			}
			if c.BytesRead, err = strconv.ParseUint(f[5], 10, 64); err == nil {
				c.BytesWritten, err = strconv.ParseUint(f[6], 10, 64)
			}
		case "READ:":
			c.Reads, c.ReadMs, err = parseOp(f)
		case "WRITE:":
			c.Writes, c.WriteMs, err = parseOp(f)
		}
		if err != nil {
			return nil, fmt.Errorf("mountstats line %d: %w", line, err)
		}
		// Time summed over all requests / elapsed = average in flight, as
		// diskstats' weighted time.
		c.WeightedMs = c.ReadMs + c.WriteMs
		stats[mountPoint] = c
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cannot read mountstats: %w", err)
	}
	return stats, nil
}

// parseOp returns the op count and cumulative execute time of a per-op line.
func parseOp(f []string) (ops, executeMs uint64, err error) {
	if len(f) < 9 {
		return 0, 0, fmt.Errorf("%s want at least 8 counters, got %d", f[0], len(f)-1)
	}
	if ops, err = strconv.ParseUint(f[1], 10, 64); err != nil {
		return 0, 0, err
	}
	if executeMs, err = strconv.ParseUint(f[8], 10, 64); err != nil {
		return 0, 0, err
	}
	return ops, executeMs, nil
}
