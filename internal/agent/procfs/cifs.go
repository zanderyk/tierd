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
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParseCifsStats parses /proc/fs/cifs/Stats into counters keyed by share,
// written the way mountinfo shows it ("//host/share"). cifs has no timing
// counters, so HasLatency and HasUtilization are false.
//
//  1. \\host\share
//     Bytes read: 0  Bytes written: 4875878400
//     Reads: 0 total 0 failed
//     Writes: 4956 total 0 failed
func ParseCifsStats(r io.Reader) (map[string]Counters, error) {
	stats := make(map[string]Counters)
	var share string
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		f := strings.Fields(text)

		// A share header: "1) \\host\share".
		if len(f) >= 2 && strings.HasSuffix(f[0], ")") && strings.HasPrefix(f[1], `\\`) {
			share = strings.ReplaceAll(f[1], `\`, "/")
			stats[share] = Counters{}
			continue
		}
		if share == "" {
			continue // global section before the first share
		}

		c := stats[share]
		var err error
		switch {
		case strings.HasPrefix(text, "Bytes read:"):
			if len(f) < 6 {
				return nil, fmt.Errorf("cifs stats line %d: want bytes read and written, got %q", line, text)
			}
			if c.BytesRead, err = strconv.ParseUint(f[2], 10, 64); err == nil {
				c.BytesWritten, err = strconv.ParseUint(f[5], 10, 64)
			}
		case strings.HasPrefix(text, "Reads:") && len(f) >= 2:
			c.Reads, err = strconv.ParseUint(f[1], 10, 64)
		case strings.HasPrefix(text, "Writes:") && len(f) >= 2:
			c.Writes, err = strconv.ParseUint(f[1], 10, 64)
		}
		if err != nil {
			return nil, fmt.Errorf("cifs stats line %d: %w", line, err)
		}
		stats[share] = c
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cannot read cifs stats: %w", err)
	}
	return stats, nil
}
