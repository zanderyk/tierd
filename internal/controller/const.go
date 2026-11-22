package controller

const (
	// ManualOverrideAnnotation on a PVC pauses tierd for it. tierd adds it
	// when someone changes the tier by hand; removing it resumes tierd.
	ManualOverrideAnnotation = "tierd.zanderyk.github.io/manual-override"

	// SkipVolumeTierStateAnnotation skips a specific pvc from being tiered
	// if added as an annotation
	SkipVolumeTierStateAnnotation = "tierd.zanderyk.github.io/skip-volume-tier"
)

const (
	finalizerName = "tierd.zanderyk.github.io/finalizer"
)
