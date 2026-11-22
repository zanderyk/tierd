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

const (
	// ManualOverrideAnnotation on a PVC pauses tierd for it. tierd adds it
	// when someone changes the tier by hand; removing it resumes tierd.
	ManualOverrideAnnotation = "tierd.zanderyk.github.io/manual-override"

	// AppliedTierAnnotation on a PVC is the tier tierd last set on it,
	// written in the same patch as the tier. A spec tier that differs from it
	// was changed by hand.
	AppliedTierAnnotation = "tierd.zanderyk.github.io/applied-tier"

	// SkipVolumeTierStateAnnotation skips a specific pvc from being tiered
	// if added as an annotation
	SkipVolumeTierStateAnnotation = "tierd.zanderyk.github.io/skip-volume-tier"
)

const (
	finalizerName = "tierd.zanderyk.github.io/finalizer"
)
