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
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
)

// statePolicyRefIndex indexes VolumeTierStates by status.policyRef, so a
// policy change can find the states it manages.
const statePolicyRefIndex = "status.policyRef"

// ConditionVTSManaged reports whether a policy is actively managing the PVC.
const ConditionVTSManaged = "Managed"

const (
	ConditionReasonPolicyMatched = "PolicyMatched"
	ConditionReasonInvalidPVC    = "InvalidPVC"
	ConditionReasonMissingPolicy = "MissingPolicy"
)

// errPVCNotBound means the PVC exists but has not bound to a volume yet.
var errPVCNotBound = errors.New("PVC is not bound")

// VolumeTierStateReconciler reconciles a VolumeTierState object
type VolumeTierStateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierstates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierstates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierstates/finalizers,verbs=update
// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumeiosamples,verbs=get;list;watch

func (r *VolumeTierStateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	vts := &tierdv1.VolumeTierState{}
	if err := r.Get(ctx, req.NamespacedName, vts); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to get VolumeTierState", "name", req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	base := vts.DeepCopy()

	pvc, err := r.ValidatePVC(ctx, req.NamespacedName)
	if err != nil && !apierrors.IsNotFound(err) && !errors.Is(err, errPVCNotBound) {
		return ctrl.Result{}, err
	}
	if err != nil {
		logger.Error(err, "PVC validation failed")
		setManaged(vts, metav1.ConditionFalse, ConditionReasonInvalidPVC, err.Error())
		if err := r.patchStatus(ctx, vts, base); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Set PVC's current VolumeAttributesClass
	if pvc.Status.CurrentVolumeAttributesClassName != nil {
		vts.Status.CurrentTier = *pvc.Status.CurrentVolumeAttributesClassName
	}

	policyName := vts.Status.PolicyRef

	if policyName == "" {
		err := fmt.Errorf("policy ref is empty")
		logger.Error(err, "VTS policy ref is empty")
		setManaged(vts, metav1.ConditionFalse, ConditionReasonMissingPolicy, err.Error())
		if err := r.patchStatus(ctx, vts, base); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	vtp := &tierdv1.VolumeTierPolicy{}
	key := types.NamespacedName{Namespace: req.Namespace, Name: policyName}
	if err := r.Get(ctx, key, vtp); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Policy not found", "policyName", policyName)
			setManaged(vts, metav1.ConditionFalse, ConditionReasonMissingPolicy,
				fmt.Sprintf("VolumeTierPolicy %s not found", policyName))
			if err := r.patchStatus(ctx, vts, base); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get VolumeTierPolicy", "policyName", policyName)
		return ctrl.Result{}, err
	}

	vioSample := &tierdv1.VolumeIOSample{}
	if err := r.Get(ctx, req.NamespacedName, vioSample); err != nil {
		if !apierrors.IsNotFound(err) {
			logger.Error(err, "Failed to get VolumeIOSample", "name", req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	dir, why := r.Evaluate(vtp, vts.Status.CurrentTier, vioSample)
	requeue := recommend(&vts.Status, vtp.Spec, dir, why, time.Now())

	setManaged(vts, metav1.ConditionTrue, ConditionReasonPolicyMatched,
		fmt.Sprintf("Managed by VolumeTierPolicy %s", vts.Status.PolicyRef))
	if err := r.patchStatus(ctx, vts, base); err != nil {
		return ctrl.Result{}, err
	}

	// Come back when the pending window ends, even if no new sample arrives.
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// Evaluate runs the logic for a VolumeTierState deciding if there should
// be action taken:
//   - Scale up if _any_ threshold is exceeded
//   - Scale down if _all_ conditions hold
func (r *VolumeTierStateReconciler) Evaluate(vtp *tierdv1.VolumeTierPolicy,
	currentTier string, sample *tierdv1.VolumeIOSample) (tierdv1.ScaleDirection, string) {
	i := tierIndex(vtp.Spec, currentTier)
	if i < 0 {
		return tierdv1.ScaleDirectionNone, "current tier is not in the policy"
	}
	return wantDirection(vtp.Spec, i, sample.Status)
}

func (r *VolumeTierStateReconciler) patchStatus(ctx context.Context, vts, base *tierdv1.VolumeTierState) error {
	if err := r.Status().Patch(ctx, vts, client.MergeFrom(base)); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to patch VolumeTierState status")
		return fmt.Errorf("cannot patch VTS status: %w", err)
	}
	return nil
}

func (r *VolumeTierStateReconciler) ValidatePVC(ctx context.Context, nsName types.NamespacedName) (*corev1.PersistentVolumeClaim, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, nsName, pvc); err != nil {
		return nil, fmt.Errorf("cannot get pvc: %w", err)
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		return nil, fmt.Errorf("PVC %s: %w", nsName.Name, errPVCNotBound)
	}
	return pvc, nil
}

func setManaged(vts *tierdv1.VolumeTierState, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&vts.Status.Conditions, metav1.Condition{
		Type:               ConditionVTSManaged,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: vts.Generation,
	})
}

// statesForPolicy maps a VolumeTierPolicy to the VolumeTierStates it manages.
func (r *VolumeTierStateReconciler) statesForPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	states := &tierdv1.VolumeTierStateList{}
	if err := r.List(
		ctx,
		states,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{statePolicyRefIndex: obj.GetName()},
	); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list VolumeTierStates for VolumeTierPolicy", "name", obj.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(states.Items))
	for _, st := range states.Items {
		reqs = append(reqs, reconcile.Request{Namespace: st.Namespace, Name: st.Name})
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *VolumeTierStateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &tierdv1.VolumeTierState{}, statePolicyRefIndex,
		func(obj client.Object) []string {
			return []string{obj.(*tierdv1.VolumeTierState).Status.PolicyRef}
		}); err != nil {
		return fmt.Errorf("cannot index VolumeTierState policyRef: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&tierdv1.VolumeTierState{}).
		Named("volumetierstate").
		Watches(&corev1.PersistentVolumeClaim{}, &handler.EnqueueRequestForObject{}).
		Watches(&tierdv1.VolumeIOSample{}, &handler.EnqueueRequestForObject{}).
		Watches(&tierdv1.VolumeTierPolicy{}, handler.EnqueueRequestsFromMapFunc(r.statesForPolicy)).
		Complete(r)
}
