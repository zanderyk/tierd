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

package v1alpha1

import (
	"fmt"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type VolumeTierPolicyMode string

const (
	// VolumeTierPolicyModeRecommend tells the VolumeTierPolicy only to send a recommendation/warning.
	VolumeTierPolicyModeRecommend VolumeTierPolicyMode = "Recommend"
	// VolumeTierPolicyModeAct tells the VolumeTierPolicy to alter the PVC's
	VolumeTierPolicyModeAct VolumeTierPolicyMode = "Act"
)

// ScaleUp defines when a PVC moves to the next higher tier. Any one
// configured trigger that holds continuously for the `for` window is
// enough to scale up.
// +kubebuilder:validation:XValidation:rule="has(self.ioPressureAbove) || has(self.latencyP95AboveMs) || has(self.utilizationAbove)",message="at least one of ioPressureAbove, latencyP95AboveMs or utilizationAbove must be set"
type ScaleUp struct {
	// ioPressureAbove triggers a scale-up when the pod's I/O pressure
	// (PSI "some" avg60, in percent) exceeds this value.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	IOPressureAbove *int32 `json:"ioPressureAbove,omitempty"`
	// latencyP95AboveMs triggers a scale-up when p95 I/O latency on the
	// volume exceeds this many milliseconds.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	LatencyP95AboveMs *int64 `json:"latencyP95AboveMs,omitempty"`
	// utilizationAbove triggers a scale-up when usage exceeds this percentage
	// of the current tier's declared ceilings. Ignored for tiers without
	// ceilings.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	UtilizationAbove *int32 `json:"utilizationAbove,omitempty"`
	// for is how long a trigger must hold continuously before scaling up.
	//
	// +optional
	// +kubebuilder:default="10m"
	For metav1.Duration `json:"for,omitempty"`
}

// ScaleDown defines when a PVC moves to the next lower tier. All configured
// conditions must hold continuously for the `for` window.
type ScaleDown struct {
	// utilizationBelow allows a scale-down when usage stays below this
	// percentage of the next lower tier's declared ceilings, so the volume
	// would still fit after moving. If that tier has no ceilings, device
	// utilization (busy time) is used instead.
	//
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	UtilizationBelow int32 `json:"utilizationBelow"`
	// ioPressureBelow additionally requires I/O pressure (PSI "some" avg60,
	// in percent) to stay below this value.
	//
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	IOPressureBelow *int32 `json:"ioPressureBelow,omitempty"`
	// For is how long the conditions must hold before scaling down. Keep it
	// much longer than scaleUp.for to avoid flapping.
	//
	// +optional
	// +kubebuilder:default="24h"
	For metav1.Duration `json:"for,omitempty"`
}

type Ceiling struct {
	// iops is the tier's maximum IOPS.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	IOPS *int64 `json:"iops,omitempty"`
	// throughputMiBps is the tier's maximum throughput in MiB/s.
	//
	// +optional
	// +kubebuilder:validation:Minimum=1
	ThroughputMiBPS *int64 `json:"throughputMiBps,omitempty"`
}

type Tier struct {
	// vacName is the name of the VolumeAttributesClass.
	VACName string `json:"name"`
	// ceilings optionally declares this tier's performance limits. If unset, saturation is inferred from latency and I/O pressure.
	//
	// +optional
	Ceilings *Ceiling `json:"ceilings"`
}

func (t *Tier) String() string {
	var iops, tp string
	if t.Ceilings != nil {
		if t.Ceilings.IOPS != nil {
			iops = strconv.FormatInt(*t.Ceilings.IOPS, 10)
		}
		if t.Ceilings.ThroughputMiBPS != nil {
			tp = strconv.FormatInt(*t.Ceilings.ThroughputMiBPS, 10)
		}
	}
	return fmt.Sprintf("name=%s,iops=%s,tpmibps=%s", t.VACName, iops, tp)
}

// +kubebuilder:validation:XValidation:rule="!has(self.scaleDown) || !has(self.scaleUp.utilizationAbove) || self.scaleDown.utilizationBelow < self.scaleUp.utilizationAbove",message="scaleDown.utilizationBelow must be lower than scaleUp.utilizationAbove"
//
// VolumeTierPolicySpec defines the desired state of VolumeTierPolicy
type VolumeTierPolicySpec struct {
	// selector decides which PVCs a VolumeTierPolicy manages. If empty,
	// all PVCs in the namespace will be selected.
	//
	// +required
	Selector metav1.LabelSelector `json:"selector"`
	// mode decides whether the PVC(s) will be patched or reported on.
	//
	// +kubebuilder:default=Recommend
	// +kubebuilder:validation:Enum=Recommend;Act
	Mode VolumeTierPolicyMode `json:"mode"`
	// tiers are the ordered list of performance levels a policy can move a PVC
	// between, from lowest to highest.
	//
	// +kubebuilder:validation:MinItems=2
	// +listType=map
	// +listMapKey=name
	Tiers []Tier `json:"tiers"`
	// scaleUp defines when a PVC moves to the next higher tier.
	//
	// +required
	ScaleUp ScaleUp `json:"scaleUp"`
	// scaleDown defines when a PVC moves to the next lower tier.
	// If unset, the policy never scales down.
	//
	// +optional
	ScaleDown *ScaleDown `json:"scaleDown,omitempty"`
}

// VolumeTierPolicyStatus defines the observed state of VolumeTierPolicy.
type VolumeTierPolicyStatus struct {
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// pvcs is a list of PVCs this policy has matched.
	//
	// +optional
	PVCs []string `json:"pvcs,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vtp
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=".spec.mode"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Conflict",type=string,JSONPath=".status.conditions[?(@.type=='Conflict')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// VolumeTierPolicy is the Schema for the volumetierpolicies API
type VolumeTierPolicy struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is a standard object metadata
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// spec defines the desired state of VolumeTierPolicy
	//
	// +required
	Spec VolumeTierPolicySpec `json:"spec"`
	// status defines the observed state of VolumeTierPolicy
	//
	// +optional
	Status VolumeTierPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VolumeTierPolicyList contains a list of VolumeTierPolicy
type VolumeTierPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VolumeTierPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VolumeTierPolicy{}, &VolumeTierPolicyList{})
		return nil
	})
}
