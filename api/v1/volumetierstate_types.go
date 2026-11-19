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
	"k8s.io/apimachinery/pkg/types"
)

type ScaleDirection string

const (
	ScaleDirectionUp   ScaleDirection = "Up"
	ScaleDirectionDown ScaleDirection = "Down"
	ScaleDirectionNone ScaleDirection = ""
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// InfeasibleTier records a tier the CSI driver rejected for this PVC (status.modifyVolumeStatus.status == Infeasible).
type InfeasibleTier struct {
	// name is the VolumeAttributesClass that was rejected.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// since is when the driver first rejected this tier.
	// +required
	Since metav1.Time `json:"since"`
	// retryAfter tells controller to re-reconcile
	// again
	// +required
	RetryAfter metav1.Time `json:"retryAfter"`
	// attempts counts how many times this tier has been rejected.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +required
	Attempts int32 `json:"attempts"`
	// vacUID is the UID of the VolumeAttributesClass when it was rejected.
	// If the class is deleted and recreated with new parameters, the UID
	// changes and the entry is dropped.
	// +optional
	VACUID types.UID `json:"vacUID,omitempty"`
	// capacity is the PVC's size when it was rejected. Some drivers cap
	// performance by size (e.g. - Azure Premium SSD),
	// so the entry is dropped if the PVC grows.
	// +optional
	Capacity *resource.Quantity `json:"capacity,omitempty"`
	// message is the driver's error, taken from the PVC's events, when available.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
}

// VolumeTierStateSpec defines the desired state of VolumeTierState. Nothing is defined for neither
// users nor controllers.
type VolumeTierStateSpec struct{}

// VolumeTierStateStatus defines the observed state of VolumeTierState.
type VolumeTierStateStatus struct {
	// policyRef is the VolumeTierPolicy controlling this PVC.
	PolicyRef string `json:"policyRef,omitempty"`
	// currentTier is the current active VolumeAttributeClass tier for this PVC.
	CurrentTier string `json:"currentTier,omitempty"`
	// recommendedTier what the VolumeTierPolicy would have applied had the VTP's VolumeTierPolicyMode been "Act"
	RecommendedTier string `json:"recommendedTier,omitempty"`
	// reason is the reason why the PVC's tier was changed.
	Reason string `json:"reason,omitempty"`
	// pendingDirection is the direction the latest sample pushes the PVC
	// in, if any. It must hold for the policy's `for` window before
	// recommendedTier changes.
	// +optional
	// +kubebuilder:validation:Enum=Up;Down
	PendingDirection ScaleDirection `json:"pendingDirection,omitempty"`
	// pendingSince is when pendingDirection last changed.
	// +optional
	PendingSince *metav1.Time `json:"pendingSince,omitempty"`
	// lastModified is the time the status was last modified.
	LastModified *metav1.Time `json:"lastModified,omitempty"`
	// lastAppliedTier is the prior tier if a change has occurred.
	LastAppliedTier string `json:"lastAppliedTier,omitempty"`
	// infeasibleTiers a list of tiers attempted to move the PVC to but unsuccessfully.
	InfeasibleTiers []InfeasibleTier `json:"infeasibleTiers,omitempty"`
	// name + since, so the backoff expires
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:selectablefield:JSONPath=".status.policyRef"

// VolumeTierState is the Schema for the volumetierstates API
type VolumeTierState struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// spec defines the desired state of VolumeTierState
	// +required
	Spec VolumeTierStateSpec `json:"spec"`
	// status defines the observed state of VolumeTierState
	// +optional
	Status VolumeTierStateStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VolumeTierStateList contains a list of VolumeTierState
type VolumeTierStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VolumeTierState `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VolumeTierState{}, &VolumeTierStateList{})
		return nil
	})
}
