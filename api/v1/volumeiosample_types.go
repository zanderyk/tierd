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

package v1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// VolumeIOSampleSpec is empty: a VolumeIOSample only carries observations,
// which live in status. The sample is named after its PVC.
type VolumeIOSampleSpec struct{}

// VolumeIOSampleStatus holds I/O figures for one PVC, aggregated by the node
// agent over the last window.
type VolumeIOSampleStatus struct {
	// nodeName is the node whose agent produced this sample.
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// pvName is the PersistentVolume bound to the PVC.
	// +optional
	PVName string `json:"pvName,omitempty"`
	// device is the block device backing the volume, as "major:minor".
	// +optional
	Device string `json:"device,omitempty"`
	// observedAt is the end of the window these figures cover.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
	// window is how long the figures were aggregated over.
	// +optional
	Window metav1.Duration `json:"window,omitempty"`
	// samples is the number of collection intervals in the window.
	// +optional
	Samples int32 `json:"samples"`
	// iops is the average completed reads plus writes per second.
	// +optional
	IOPS int64 `json:"iops"`
	// throughputBytesPerSecond is the average bytes read plus written per
	// second.
	// +optional
	ThroughputBytesPerSecond int64 `json:"throughputBytesPerSecond"`
	// latencyP50Micros is the median of the per-interval average I/O latency.
	// +optional
	LatencyP50Micros int64 `json:"latencyP50Micros"`
	// latencyP95Micros is the 95th percentile of the per-interval average I/O
	// latency. It is not a per-request percentile: the kernel only exposes
	// totals, so each interval contributes one average.
	// +optional
	LatencyP95Micros int64 `json:"latencyP95Micros"`
	// utilizationPercent is the share of the window the device had I/O in
	// flight (diskstats io_ticks).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	UtilizationPercent int32 `json:"utilizationPercent"`
	// queueDepth is the average number of requests in flight.
	// +optional
	QueueDepth *resource.Quantity `json:"queueDepth,omitempty"`
	// ioPressureSomeAvg60 is the cgroup v2 io.pressure "some" avg60, in
	// percent, of the pod using the volume. Pressure is per pod, not per
	// device; if several pods use the volume, the highest is reported.
	// +optional
	IOPressureSomeAvg60 *resource.Quantity `json:"ioPressureSomeAvg60,omitempty"`
	// conditions represent the current state of the VolumeIOSample resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".status.nodeName"
// +kubebuilder:printcolumn:name="IOPS",type=integer,JSONPath=".status.iops"
// +kubebuilder:printcolumn:name="P95us",type=integer,JSONPath=".status.latencyP95Micros"
// +kubebuilder:printcolumn:name="Util%",type=integer,JSONPath=".status.utilizationPercent"
// +kubebuilder:printcolumn:name="Observed",type=date,JSONPath=".status.observedAt"

// VolumeIOSample is the Schema for the volumeiosamples API
type VolumeIOSample struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of VolumeIOSample
	// +optional
	Spec VolumeIOSampleSpec `json:"spec,omitzero"`

	// status defines the observed state of VolumeIOSample
	// +optional
	Status VolumeIOSampleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VolumeIOSampleList contains a list of VolumeIOSample
type VolumeIOSampleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VolumeIOSample `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VolumeIOSample{}, &VolumeIOSampleList{})
		return nil
	})
}
