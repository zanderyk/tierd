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

package builder

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	tierdv1alpha1 "github.com/zanderyk/tierd/api/v1alpha1"
)

type VolumeTierPolicyBuilder struct {
	object *tierdv1alpha1.VolumeTierPolicy
}

// Application is the constructor for a VolumeTierPolicyBuilder.
func VolumeTierPolicy(namespace, name string) *VolumeTierPolicyBuilder {
	return &VolumeTierPolicyBuilder{
		object: &tierdv1alpha1.VolumeTierPolicy{
			APIVersion: tierdv1alpha1.GroupVersion.String(),
			Kind:       "VolumeTierPolicy",
			Namespace:  namespace,
			Name:       name,
		},
	}
}

// Build returns the built VolumeTierPolicy.
func (b *VolumeTierPolicyBuilder) Build() *tierdv1alpha1.VolumeTierPolicy { return b.object }

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
