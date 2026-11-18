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
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
)

func TestWantDirection(t *testing.T) {
	// Same shape as hack/aks-smoke/policy.yaml.
	spec := tierdv1.VolumeTierPolicySpec{
		Tiers: []tierdv1.Tier{
			{VACName: "base", Ceilings: &tierdv1.Ceiling{IOPS: new(int64(3000)), ThroughputMiBPS: new(int64(125))}},
			{VACName: "fast", Ceilings: &tierdv1.Ceiling{IOPS: new(int64(5000)), ThroughputMiBPS: new(int64(200))}},
		},
		ScaleUp: tierdv1.ScaleUp{
			IOPressureAbove:   new(int32(20)),
			LatencyP95AboveMs: new(int64(20)),
			UtilizationAbove:  new(int32(80)),
		},
		ScaleDown: &tierdv1.ScaleDown{UtilizationBelow: 30, IOPressureBelow: new(int32(1))},
	}
	psi := func(v string) *resource.Quantity { return new(resource.MustParse(v)) }

	tests := []struct {
		name   string
		tier   int
		sample tierdv1.VolumeIOSampleStatus
		want   scaleDirection
	}{
		{"idle on base stays", 0, tierdv1.VolumeIOSampleStatus{IOPS: 10, IOPressureSomeAvg60: psi("0")}, scaleDirectionNone},
		{"iops near base ceiling", 0, tierdv1.VolumeIOSampleStatus{IOPS: 3060}, scaleDirectionUp},
		{"throughput near base ceiling", 0, tierdv1.VolumeIOSampleStatus{ThroughputBytesPerSecond: 110 << 20}, scaleDirectionUp},
		{"high latency", 0, tierdv1.VolumeIOSampleStatus{LatencyP95Micros: 25_000}, scaleDirectionUp},
		{"high pressure", 0, tierdv1.VolumeIOSampleStatus{IOPressureSomeAvg60: psi("35")}, scaleDirectionUp},
		{"busy on top tier stays", 1, tierdv1.VolumeIOSampleStatus{IOPS: 4900}, scaleDirectionNone},
		{"quiet on fast goes down", 1, tierdv1.VolumeIOSampleStatus{IOPS: 300, IOPressureSomeAvg60: psi("0")}, scaleDirectionDown},
		{"too busy for base stays", 1, tierdv1.VolumeIOSampleStatus{IOPS: 2000, IOPressureSomeAvg60: psi("0")}, scaleDirectionNone},
		{"unknown pressure blocks down", 1, tierdv1.VolumeIOSampleStatus{IOPS: 300}, scaleDirectionNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := wantDirection(spec, tt.tier, tt.sample)
			if got != tt.want {
				t.Errorf("got %q (%s), want %q", got, why, tt.want)
			}
		})
	}
}
