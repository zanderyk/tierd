package controller

const (
	finalizerName = "tierd.zanderyk.github.io/finalizer"

	// ManualOverrideAnnotation on a PVC pauses tierd for it. tierd adds it
	// when someone changes the tier by hand; removing it resumes tierd.
	ManualOverrideAnnotation = "tierd.zanderyk.github.io/manual-override"
)
