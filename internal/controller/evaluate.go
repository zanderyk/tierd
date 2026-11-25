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

package controller

import (
	"fmt"
	"slices"
	"time"

	tierdv1alpha1 "github.com/zanderyk/tierd/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// wantDirection reports which way the sample pushes a PVC on tiers[tier]
// right now, and why. It ignores the `for` windows.
func wantDirection(spec tierdv1alpha1.VolumeTierPolicySpec, tier int, s tierdv1alpha1.VolumeIOSampleStatus) (tierdv1alpha1.ScaleDirection, string) {
	if tier < len(spec.Tiers)-1 {
		if why, ok := scaleUpHolds(spec.ScaleUp, spec.Tiers[tier], s); ok {
			return tierdv1alpha1.ScaleDirectionUp, why
		}
	}
	if spec.ScaleDown != nil && tier > 0 {
		if why, ok := scaleDownHolds(*spec.ScaleDown, spec.Tiers[tier-1], s); ok {
			return tierdv1alpha1.ScaleDirectionDown, why
		}
	}
	return tierdv1alpha1.ScaleDirectionNone, ""
}

// scaleUpHolds is true if any configured trigger holds.
func scaleUpHolds(up tierdv1alpha1.ScaleUp, current tierdv1alpha1.Tier, s tierdv1alpha1.VolumeIOSampleStatus) (string, bool) {
	if up.IOPressureAbove != nil {
		if p, ok := pressure(s); ok && p > float64(*up.IOPressureAbove) {
			return fmt.Sprintf("I/O pressure %.0f%% > %d%%", p, *up.IOPressureAbove), true
		}
	}
	if up.LatencyP95AboveMs != nil && s.LatencyP95Micros != nil {
		if ms := *s.LatencyP95Micros / 1000; ms > *up.LatencyP95AboveMs {
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
// none. Unknown pressure or utilization blocks a scale-down.
func scaleDownHolds(down tierdv1alpha1.ScaleDown, lower tierdv1alpha1.Tier, s tierdv1alpha1.VolumeIOSampleStatus) (string, bool) {
	pct, ok := ceilingPercent(lower.Ceilings, s)
	if !ok {
		if s.UtilizationPercent == nil {
			return "", false
		}
		pct = float64(*s.UtilizationPercent)
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
func ceilingPercent(c *tierdv1alpha1.Ceiling, s tierdv1alpha1.VolumeIOSampleStatus) (float64, bool) {
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
func pressure(s tierdv1alpha1.VolumeIOSampleStatus) (float64, bool) {
	if s.IOPressureSomeAvg60 == nil {
		return 0, false
	}
	return s.IOPressureSomeAvg60.AsApproximateFloat64(), true
}

// recommend applies the `for` window to dir and updates st.PendingDirection,
// st.PendingSince, st.RecommendedTier and st.Reason. It returns how long until
// the pending window ends, or 0 if nothing is pending.
//   - direction changed → save it, PendingSince = now
//   - direction None → recommend current tier, return 0
//   - waited < `for` → recommend current tier, return time left
//   - waited >= `for` → recommend next tier in dir, return 0
func recommend(
	st *tierdv1alpha1.VolumeTierStateStatus,
	spec tierdv1alpha1.VolumeTierPolicySpec,
	direction tierdv1alpha1.ScaleDirection,
	why string,
	now time.Time,
) time.Duration {
	if direction != st.PendingDirection {
		st.PendingDirection = direction
		st.PendingSince = new(metav1.NewTime(now))
	}
	if direction == tierdv1alpha1.ScaleDirectionNone {
		st.PendingSince = nil
		st.RecommendedTier = st.CurrentTier
		st.Reason = ""
		return 0
	}

	window := spec.ScaleUp.For.Duration
	if direction == tierdv1alpha1.ScaleDirectionDown {
		window = spec.ScaleDown.For.Duration
	}

	waited := now.Sub(st.PendingSince.Time)
	if waited < window {
		st.RecommendedTier = st.CurrentTier
		st.Reason = fmt.Sprintf("%s for %s of %s", why, waited.Round(time.Second), window)
		return window - waited
	}

	i := tierIndex(spec, st.CurrentTier)
	if direction == tierdv1alpha1.ScaleDirectionUp {
		i++
	} else {
		i--
	}
	next := spec.Tiers[i].VACName
	if until, ok := backoffUntil(*st, next); ok && now.Before(until) {
		st.RecommendedTier = st.CurrentTier
		st.Reason = fmt.Sprintf("%s, but %s failed; retrying after %s", why, next, until.UTC().Format(time.RFC3339))
		return until.Sub(now)
	}
	st.RecommendedTier = next
	st.Reason = fmt.Sprintf("%s for %s", why, waited.Round(time.Second))
	return 0
}

// tierIndex is the position of the named tier in spec.Tiers, or -1.
func tierIndex(spec tierdv1alpha1.VolumeTierPolicySpec, name string) int {
	return slices.IndexFunc(spec.Tiers, func(t tierdv1alpha1.Tier) bool { return t.VACName == name })
}

// pickTier chooses the starting tier for a PVC with no VolumeAttributesClass:
// the lowest tier whose ceilings fit the sample's load under the scale-up
// threshold (100% if none is set), or the highest tier with ceilings if
// none fits. ok is false when no tier declares ceilings, since the load
// can't be compared then.
func pickTier(spec tierdv1alpha1.VolumeTierPolicySpec, s tierdv1alpha1.VolumeIOSampleStatus) (tier, why string, ok bool) {
	limit := 100.0
	if spec.ScaleUp.UtilizationAbove != nil {
		limit = float64(*spec.ScaleUp.UtilizationAbove)
	}
	highest := ""
	for _, t := range spec.Tiers {
		pct, hasCeilings := ceilingPercent(t.Ceilings, s)
		if !hasCeilings {
			continue
		}
		if pct < limit {
			return t.VACName, fmt.Sprintf("adopting: load is %.0f%% of %s", pct, t.VACName), true
		}
		highest = t.VACName
	}
	if highest == "" {
		return "", "", false
	}
	return highest, fmt.Sprintf("adopting: load exceeds every tier, using the highest, %s", highest), true
}
