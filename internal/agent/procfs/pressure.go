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

// PressureLine is one line ("some" or "full") of a PSI file.
type PressureLine struct {
	// Avg10, Avg60 and Avg300 are percentages of wall time stalled.
	Avg10, Avg60, Avg300 float64
	// TotalMicros is the cumulative stall time in microseconds.
	TotalMicros uint64
}

// Pressure is the content of a cgroup v2 io.pressure (or /proc/pressure/io)
// file. Full is zero if the kernel does not report it.
type Pressure struct {
	Some PressureLine
	Full PressureLine
}

// ParsePressure parses the PSI format:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
func ParsePressure(r io.Reader) (Pressure, error) {
	var p Pressure
	var sawSome bool
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		var dst *PressureLine
		switch f[0] {
		case "some":
			dst, sawSome = &p.Some, true
		case "full":
			dst = &p.Full
		default:
			return Pressure{}, fmt.Errorf("pressure: unknown line kind %q", f[0])
		}
		for _, kv := range f[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return Pressure{}, fmt.Errorf("pressure: malformed field %q", kv)
			}
			var err error
			switch k {
			case "avg10":
				dst.Avg10, err = strconv.ParseFloat(v, 64)
			case "avg60":
				dst.Avg60, err = strconv.ParseFloat(v, 64)
			case "avg300":
				dst.Avg300, err = strconv.ParseFloat(v, 64)
			case "total":
				dst.TotalMicros, err = strconv.ParseUint(v, 10, 64)
			}
			if err != nil {
				return Pressure{}, fmt.Errorf("pressure: field %q: %w", kv, err)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Pressure{}, fmt.Errorf("cannot read pressure: %w", err)
	}
	if !sawSome {
		return Pressure{}, fmt.Errorf("pressure: no \"some\" line")
	}
	return p, nil
}
