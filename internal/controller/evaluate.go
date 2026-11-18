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

package controller

import (
	"fmt"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
)

type scaleDirection string

const (
	scaleDirectionUp   scaleDirection = "Up"
	scaleDirectionDown scaleDirection = "Down"
	scaleDirectionNone scaleDirection = ""
)

// wantDirection reports which way the sample pushes a PVC on tiers[tier]
// right now, and why. It ignores the `for` windows.
func wantDirection(spec tierdv1.VolumeTierPolicySpec, tier int, s tierdv1.VolumeIOSampleStatus) (scaleDirection, string) {
	if tier < len(spec.Tiers)-1 {
		if why, ok := scaleUpHolds(spec.ScaleUp, spec.Tiers[tier], s); ok {
			return scaleDirectionUp, why
		}
	}
	if spec.ScaleDown != nil && tier > 0 {
		if why, ok := scaleDownHolds(*spec.ScaleDown, spec.Tiers[tier-1], s); ok {
			return scaleDirectionDown, why
		}
	}
	return scaleDirectionNone, ""
}

// scaleUpHolds is true if any configured trigger holds.
func scaleUpHolds(up tierdv1.ScaleUp, current tierdv1.Tier, s tierdv1.VolumeIOSampleStatus) (string, bool) {
	if up.IOPressureAbove != nil {
		if p, ok := pressure(s); ok && p > float64(*up.IOPressureAbove) {
			return fmt.Sprintf("I/O pressure %.0f%% > %d%%", p, *up.IOPressureAbove), true
		}
	}
	if up.LatencyP95AboveMs != nil {
		if ms := s.LatencyP95Micros / 1000; ms > *up.LatencyP95AboveMs {
			return fmt.Sprintf("p95 latency %dms > %dms", ms, *up.LatencyP95AboveMs), true
		}
	}
	if up.UtilizationAbove != nil {
		if pct, ok := ceilingPercent(current.Ceilings, s); ok && pct > float64(*up.UtilizationAbove) {
			return fmt.Sprintf("utilization %.0f%% of %s > %d%%", pct, current.VACName, *up.UtilizationAbove), true
		}
	}
	return "", false
}

// scaleDownHolds is true if every configured condition holds. Utilization is
// measured against the lower tier's ceilings, or device busy time if it has
// none. Unknown pressure blocks a scale-down.
func scaleDownHolds(down tierdv1.ScaleDown, lower tierdv1.Tier, s tierdv1.VolumeIOSampleStatus) (string, bool) {
	pct, ok := ceilingPercent(lower.Ceilings, s)
	if !ok {
		pct = float64(s.UtilizationPercent)
	}
	if pct >= float64(down.UtilizationBelow) {
		return "", false
	}
	if down.IOPressureBelow != nil {
		if p, ok := pressure(s); !ok || p >= float64(*down.IOPressureBelow) {
			return "", false
		}
	}
	return fmt.Sprintf("utilization %.0f%% of %s < %d%%", pct, lower.VACName, down.UtilizationBelow), true
}

// ceilingPercent is usage as a percentage of the tier's declared ceilings,
// taking the higher of IOPS and throughput. ok is false without ceilings.
func ceilingPercent(c *tierdv1.Ceiling, s tierdv1.VolumeIOSampleStatus) (float64, bool) {
	if c == nil || (c.IOPS == nil && c.ThroughputMiBPS == nil) {
		return 0, false
	}
	var pct float64
	if c.IOPS != nil {
		pct = float64(s.IOPS) / float64(*c.IOPS) * 100
	}
	if c.ThroughputMiBPS != nil {
		pct = max(pct, float64(s.ThroughputBytesPerSecond)/float64(*c.ThroughputMiBPS<<20)*100)
	}
	return pct, true
}

// pressure is the sample's PSI IOPressureSomeAvg60 in percent; ok is false if unknown.
func pressure(s tierdv1.VolumeIOSampleStatus) (float64, bool) {
	if s.IOPressureSomeAvg60 == nil {
		return 0, false
	}
	return s.IOPressureSomeAvg60.AsApproximateFloat64(), true
}
