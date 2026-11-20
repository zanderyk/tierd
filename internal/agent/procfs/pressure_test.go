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
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

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

func TestParsePressureSomeOnly(t *testing.T) {
	// /proc/pressure/io on older kernels and some cgroup files report only
	// the "some" line; Full stays zero.
	p, err := ParsePressure(strings.NewReader("some avg10=1.50 avg60=2.00 avg300=3.25 total=42\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Pressure{Some: PressureLine{Avg10: 1.5, Avg60: 2, Avg300: 3.25, TotalMicros: 42}}
	if p != want {
		t.Errorf("got %+v, want %+v", p, want)
	}
}

func TestParsePressureIgnoresUnknownKeys(t *testing.T) {
	p, err := ParsePressure(strings.NewReader("some avg10=1.00 future=7 total=5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Some.Avg10 != 1 || p.Some.TotalMicros != 5 {
		t.Errorf("got %+v", p)
	}
}

func TestParsePressureErrors(t *testing.T) {
	tests := []struct {
		name, in string
	}{
		{"empty", ""},
		{"unknown line kind", "some avg10=0 total=0\nhalf avg10=0 total=0\n"},
		{"field without =", "some avg10 total=0\n"},
		{"bad float", "some avg10=abc total=0\n"},
		{"bad total", "some avg10=0.00 total=-1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParsePressure(strings.NewReader(tt.in)); err == nil {
				t.Errorf("want error for %q", tt.in)
			}
		})
	}
}

func TestParsePressureReadError(t *testing.T) {
	if _, err := ParsePressure(iotest.ErrReader(errors.New("boom"))); err == nil {
		t.Error("want error when the reader fails")
	}
}
