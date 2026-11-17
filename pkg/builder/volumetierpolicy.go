package builder

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
)

type VolumeTierPolicyBuilder struct{ object *tierdv1.VolumeTierPolicy }

// Application is the constructor for a VolumeTierPolicyBuilder.
func VolumeTierPolicy(namespace, name string) *VolumeTierPolicyBuilder {
	return &VolumeTierPolicyBuilder{
		object: &tierdv1.VolumeTierPolicy{
			APIVersion: tierdv1.GroupVersion.String(),
			Kind:       "VolumeTierPolicy",
			Namespace:  namespace,
			Name:       name,
		},
	}
}

// Build returns the built VolumeTierPolicy.
func (b *VolumeTierPolicyBuilder) Build() *tierdv1.VolumeTierPolicy { return b.object }

// ObjectMeta applies functional options to the Application's ObjectMeta.
func (b *VolumeTierPolicyBuilder) ObjectMeta(opts ...ObjectMetaOpt) *VolumeTierPolicyBuilder {
	for _, opt := range opts {
		opt(b.object)
	}
	return b
}

func (b *VolumeTierPolicyBuilder) Unstructured() *unstructured.Unstructured {
	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(b.object)
	if err != nil {
		return nil
	}
	return &unstructured.Unstructured{Object: unstructuredObj}
}
