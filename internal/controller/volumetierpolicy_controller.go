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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
)

var (
	ErrNamespaceTerminating = fmt.Errorf("namespace is in terminating state")
	finalizerName           = "protect.trident.netapp.io/finalizer"
)

type ConditionType string

const (
	ConditionAppReady ConditionType = "Ready"
)

type ConditionReason string

const (
	ConditionReasonError ConditionReason = "Error"
	ConditionReasonReady ConditionReason = "Ready"
)

// VolumeTierPolicyReconciler reconciles a VolumeTierPolicy object
type VolumeTierPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierpolicies/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the VolumeTierPolicy object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/reconcile
func (r *VolumeTierPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	logger.V(2).Info("Starting Reconcile for VolumeTierPolicy")
	defer logger.V(2).Info("VolumeTierPolicy reconcile ended")

	vtp := &tierdv1.VolumeTierPolicy{}
	if err := r.Get(ctx, req.NamespacedName, vtp); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "unable to fetch VolumeTierPolicy", "name", req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if vtp.Status.Conditions == nil {
		conditionList := make([]metav1.Condition, 0)
		vtp.Status.Conditions = conditionList
	}

	selector, err := metav1.LabelSelectorAsSelector(&vtp.Spec.Selector)
	if err != nil {
		logger.Error(err, "Failed to parse selector", "selector", vtp.Spec.Selector.String())
		return ctrl.Result{}, nil
	}

	pvcs := &corev1.PersistentVolumeList{}
	if err := r.List(
		ctx,
		pvcs,
		client.InNamespace(vtp.GetNamespace()),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		logger.Error(err, "unable to list PVCs", "namespace", vtp.GetNamespace(), "selector", vtp.Spec.Selector.String())
		return ctrl.Result{}, fmt.Errorf("unable to list PVCS: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VolumeTierPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tierdv1.VolumeTierPolicy{}).
		Named("volumetierpolicy").
		Complete(r)
}
