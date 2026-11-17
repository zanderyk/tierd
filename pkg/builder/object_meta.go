package builder

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObjectMetaOpt is a functional option for ObjectMeta.
type ObjectMetaOpt func(metav1.Object)
