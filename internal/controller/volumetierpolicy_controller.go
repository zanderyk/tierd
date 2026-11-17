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
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	tierdv1 "github.com/zanderyk/tierd/api/v1"
)

type ConditionType string

const (
	ConditionVTPReady    ConditionType = "Ready"
	ConditionVTPConflict ConditionType = "Conflict"
)

type ConditionReason string

const (
	ConditionReasonError                         ConditionReason = "Error"
	ConditionReasonReady                         ConditionReason = "Ready"
	ConditionReasonInvalidTiers                  ConditionReason = "InvalidTiers"
	ConditionReasonVolumeAttributesClassNotFound ConditionReason = "VolumeAttributesClassNotFound"
	ConditionReasonDriverMismatch                ConditionReason = "DriverMismatch"
	ConditionReasonNoConflict                    ConditionReason = "NoConflict"
	ConditionReasonPolicyConflict                ConditionReason = "PolicyConflict"
)

// tierVACNameIndex indexes VolumeTierPolicies by the VAC names of their tiers,
// so a VAC event can be mapped to the policies that reference it.
const tierVACNameIndex = ".spec.tiers.name"

// VolumeTierPolicyReconciler reconciles a VolumeTierPolicy object
type VolumeTierPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// tierError is a problem with a policy's tiers that only a change to the
// policy or the cluster's VolumeAttributesClasses can fix. It is reported on
// the Ready condition instead of being retried.
type tierError struct {
	reason ConditionReason
	err    error
}

func (e *tierError) Error() string { return e.err.Error() }
func (e *tierError) Unwrap() error { return e.err }

// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.k8s.io,resources=volumeattributesclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch

// Reconcile validates a VolumeTierPolicy's tiers, reports the result on the
// Ready condition, and ensures a VolumeTierState exists for each matched PVC.
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
			logger.Error(err, "Failed to get VolumeTierPolicy", "name", req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	base := vtp.DeepCopy()

	if err := r.validateTiers(ctx, vtp.Spec.Tiers); err != nil {
		var te *tierError
		if !errors.As(err, &te) {
			// API or cache failure: retry.
			logger.Error(err, "Failed to validate VolumeTierPolicy tiers")
			return ctrl.Result{}, fmt.Errorf("cannot validate VTP tiers: %w", err)
		}
		logger.Info("Rejected VolumeTierPolicy tiers", "reason", te.reason, "error", te.Error())
		setReady(vtp, metav1.ConditionFalse, te.reason, te.Error())
		return ctrl.Result{}, r.patchStatus(ctx, vtp, base)
	}

	selector, err := metav1.LabelSelectorAsSelector(&vtp.Spec.Selector)
	if err != nil {
		logger.Error(err, "Failed to parse selector", "selector", vtp.Spec.Selector.String())
		return ctrl.Result{}, fmt.Errorf("cannot parse labelSelector: %w", err)
	}

	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(
		ctx,
		pvcs,
		client.InNamespace(vtp.GetNamespace()),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		logger.Error(err, "Failed to list PVCs", "namespace", vtp.GetNamespace(), "selector", vtp.Spec.Selector.String())
		return ctrl.Result{}, fmt.Errorf("unable to list PVCs: %w", err)
	}

	selectors, err := r.namespaceSelectors(ctx, vtp, selector)
	if err != nil {
		logger.Error(err, "Failed to list VolumeTierPolicies", "namespace", vtp.GetNamespace())
		return ctrl.Result{}, err
	}

	matched := make([]string, 0, len(pvcs.Items))
	var conflicts []string
	for _, pvc := range pvcs.Items {
		state, err := r.ensureVolumeTierState(ctx, &pvc)
		if err != nil {
			logger.Error(err, "Failed to ensure VolumeTierState", "pvc", pvc.Name)
			return ctrl.Result{}, err
		}

		// A PVC matched by more than one policy is left unmanaged. Every
		// policy involved computes the same answer, so they never fight.
		ref := vtp.Name
		if owners := matchingPolicies(selectors, pvc.Labels); len(owners) > 1 {
			ref = ""
			conflicts = append(conflicts, fmt.Sprintf("%s (%s)", pvc.Name, strings.Join(owners, ", ")))
		}
		if err := r.setPolicyRef(ctx, state, ref); err != nil {
			logger.Error(err, "Failed to set VolumeTierState policyRef", "pvc", pvc.Name)
			return ctrl.Result{}, err
		}
		matched = append(matched, pvc.Name)
	}
	slices.Sort(matched)

	setConflict(vtp, conflicts)
	vtp.Status.PVCs = matched
	setReady(vtp, metav1.ConditionTrue, ConditionReasonReady, "All tier VolumeAttributesClasses exist and share one driver")
	return ctrl.Result{}, r.patchStatus(ctx, vtp, base)
}

// ensureVolumeTierState creates the VolumeTierState for a PVC if it does not
// exist.
func (r *VolumeTierPolicyReconciler) ensureVolumeTierState(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*tierdv1.VolumeTierState, error) {
	state := &tierdv1.VolumeTierState{}
	err := r.Get(ctx, client.ObjectKeyFromObject(pvc), state)
	if err == nil {
		return state, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("cannot get VolumeTierState %s: %w", pvc.Name, err)
	}

	state = &tierdv1.VolumeTierState{Name: pvc.Name, Namespace: pvc.Namespace}
	if err := controllerutil.SetOwnerReference(pvc, state, r.Scheme); err != nil {
		return nil, fmt.Errorf("cannot set owner reference on VolumeTierState %s: %w", pvc.Name, err)
	}
	if err := r.Create(ctx, state); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("cannot create VolumeTierState %s: %w", pvc.Name, err)
		}
		// Created concurrently; fetch it so the caller has a resourceVersion.
		if err := r.Get(ctx, client.ObjectKeyFromObject(pvc), state); err != nil {
			return nil, fmt.Errorf("cannot get VolumeTierState %s: %w", pvc.Name, err)
		}
		return state, nil
	}
	logf.FromContext(ctx).Info("Created VolumeTierState", "name", state.Name)
	return state, nil
}

// setPolicyRef records in status which policy manages the state's PVC; ""
// means none.
func (r *VolumeTierPolicyReconciler) setPolicyRef(ctx context.Context, state *tierdv1.VolumeTierState, ref string) error {
	if state.Status.PolicyRef == ref {
		return nil
	}
	base := state.DeepCopy()
	state.Status.PolicyRef = ref
	if err := r.Status().Patch(ctx, state, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("cannot patch VolumeTierState %s policyRef: %w", state.Name, err)
	}
	return nil
}

// namespaceSelectors returns the parsed selector of every policy in vtp's
// namespace, keyed by policy name. Policies with an invalid selector are
// skipped; they report the error on their own Ready condition.
func (r *VolumeTierPolicyReconciler) namespaceSelectors(ctx context.Context, vtp *tierdv1.VolumeTierPolicy, own labels.Selector) (map[string]labels.Selector, error) {
	policies := &tierdv1.VolumeTierPolicyList{}
	if err := r.List(ctx, policies, client.InNamespace(vtp.Namespace)); err != nil {
		return nil, fmt.Errorf("cannot list VolumeTierPolicies: %w", err)
	}
	selectors := make(map[string]labels.Selector, len(policies.Items))
	for _, p := range policies.Items {
		if s, err := metav1.LabelSelectorAsSelector(&p.Spec.Selector); err == nil {
			selectors[p.Name] = s
		}
	}
	// The cache may not have this policy's latest selector yet.
	selectors[vtp.Name] = own
	return selectors, nil
}

// matchingPolicies returns the sorted names of the policies whose selector
// matches the given PVC labels.
func matchingPolicies(selectors map[string]labels.Selector, pvcLabels map[string]string) []string {
	var owners []string
	for name, s := range selectors {
		if s.Matches(labels.Set(pvcLabels)) {
			owners = append(owners, name)
		}
	}
	slices.Sort(owners)
	return owners
}

func setConflict(vtp *tierdv1.VolumeTierPolicy, conflicts []string) {
	cond := metav1.Condition{
		Type:               string(ConditionVTPConflict),
		Status:             metav1.ConditionFalse,
		Reason:             string(ConditionReasonNoConflict),
		Message:            "No matched PVC is matched by another VolumeTierPolicy",
		ObservedGeneration: vtp.Generation,
	}
	if len(conflicts) > 0 {
		cond.Status = metav1.ConditionTrue
		cond.Reason = string(ConditionReasonPolicyConflict)
		cond.Message = "PVCs also matched by other VolumeTierPolicies are left unmanaged: " + strings.Join(conflicts, "; ")
	}
	meta.SetStatusCondition(&vtp.Status.Conditions, cond)
}

func setReady(vtp *tierdv1.VolumeTierPolicy, status metav1.ConditionStatus, reason ConditionReason, msg string) {
	meta.SetStatusCondition(&vtp.Status.Conditions, metav1.Condition{
		Type:               string(ConditionVTPReady),
		Status:             status,
		Reason:             string(reason),
		Message:            msg,
		ObservedGeneration: vtp.Generation,
	})
}

// patchStatus sends only the status fields changed since base. This
// controller is the only writer of the policy's status, so a merge patch
// without optimistic locking is safe.
func (r *VolumeTierPolicyReconciler) patchStatus(ctx context.Context, vtp, base *tierdv1.VolumeTierPolicy) error {
	if err := r.Status().Patch(ctx, vtp, client.MergeFrom(base)); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to patch VolumeTierPolicy status")
		return fmt.Errorf("cannot patch VTP status: %w", err)
	}
	return nil
}

// validateTiers checks the tiers' spec first and, only if that passes, the
// VolumeAttributesClasses they name. Problems the user must fix are returned
// as *tierError; any other error is an API failure and should be retried.
func (r *VolumeTierPolicyReconciler) validateTiers(ctx context.Context, tiers []tierdv1.Tier) error {
	if err := validateTierSpec(tiers); err != nil {
		return &tierError{reason: ConditionReasonInvalidTiers, err: err}
	}
	return r.validateVolumeAttributesClasses(ctx, tiers)
}

// validateVolumeAttributesClasses checks that every tier's VAC exists and that
// they all use the same CSI driver. Reads come from the informer cache; a VAC
// created after a NotFound is picked up through the VAC watch.
func (r *VolumeTierPolicyReconciler) validateVolumeAttributesClasses(ctx context.Context, tiers []tierdv1.Tier) error {
	var missing []string
	drivers := make(map[string][]string) // driverName -> tier names
	for _, tier := range tiers {
		vac := &storagev1.VolumeAttributesClass{}
		if err := r.Get(ctx, client.ObjectKey{Name: tier.VACName}, vac); err != nil {
			if apierrors.IsNotFound(err) {
				missing = append(missing, tier.VACName)
				continue
			}
			return fmt.Errorf("cannot get VolumeAttributesClass %s: %w", tier.VACName, err)
		}
		drivers[vac.DriverName] = append(drivers[vac.DriverName], tier.VACName)
	}

	if len(missing) > 0 {
		return &tierError{
			reason: ConditionReasonVolumeAttributesClassNotFound,
			err:    fmt.Errorf("VolumeAttributesClasses not found: %s", strings.Join(missing, ", ")),
		}
	}
	if len(drivers) > 1 {
		parts := make([]string, 0, len(drivers))
		for driver, names := range drivers {
			parts = append(parts, fmt.Sprintf("%s (%s)", driver, strings.Join(names, ", ")))
		}
		slices.Sort(parts)
		return &tierError{
			reason: ConditionReasonDriverMismatch,
			err:    fmt.Errorf("tier VolumeAttributesClasses use different drivers: %s", strings.Join(parts, "; ")),
		}
	}
	return nil
}

// validateTierSpec checks that tiers form a usable ladder: at least two
// tiers, unique non-empty names, and ceilings that strictly increase from the
// first (lowest) tier to the last (highest), per metric. Tiers that do not
// declare a metric are skipped when comparing that metric. All problems are
// returned together.
func validateTierSpec(tiers []tierdv1.Tier) error {
	var errs []error

	if len(tiers) < 2 {
		errs = append(errs, fmt.Errorf("at least two tiers are required, got %d", len(tiers)))
	}

	type last struct {
		val  *int64
		tier string
	}
	ascending := func(prev *last, cur *int64, tier, metric string) {
		if cur == nil {
			return
		}
		if prev.val != nil && *cur <= *prev.val {
			errs = append(errs, fmt.Errorf(
				"tier %q: %s ceiling %d must be greater than tier %q (%d); tiers are ordered lowest to highest",
				tier, metric, *cur, prev.tier, *prev.val))
		}
		prev.val, prev.tier = cur, tier
	}

	var lastIOPS, lastTP last
	seen := make(map[string]struct{}, len(tiers))
	for i, tier := range tiers {
		if tier.VACName == "" {
			errs = append(errs, fmt.Errorf("tier %d: name must be set", i))
		} else if _, dup := seen[tier.VACName]; dup {
			errs = append(errs, fmt.Errorf("tier %q: duplicate name", tier.VACName))
		}
		seen[tier.VACName] = struct{}{}

		c := tier.Ceilings
		if c == nil {
			continue
		}
		if c.IOPS == nil && c.ThroughputMiBPS == nil {
			errs = append(errs, fmt.Errorf("tier %q: ceilings must set iops and/or throughputMiBps, or be omitted", tier.VACName))
		}
		ascending(&lastIOPS, c.IOPS, tier.VACName, "iops")
		ascending(&lastTP, c.ThroughputMiBPS, tier.VACName, "throughputMiBps")
	}
	return errors.Join(errs...)
}

// policiesForVAC maps a VolumeAttributesClass event to every policy that
// names it as a tier.
func (r *VolumeTierPolicyReconciler) policiesForVAC(ctx context.Context, obj client.Object) []reconcile.Request {
	policies := &tierdv1.VolumeTierPolicyList{}
	if err := r.List(ctx, policies, client.MatchingFields{tierVACNameIndex: obj.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list VolumeTierPolicies for VolumeAttributesClass", "name", obj.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(policies.Items))
	for _, p := range policies.Items {
		reqs = append(reqs, reconcile.Request{Namespace: p.Namespace, Name: p.Name})
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *VolumeTierPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &tierdv1.VolumeTierPolicy{}, tierVACNameIndex,
		func(obj client.Object) []string {
			vtp := obj.(*tierdv1.VolumeTierPolicy)
			names := make([]string, 0, len(vtp.Spec.Tiers))
			for _, t := range vtp.Spec.Tiers {
				names = append(names, t.VACName)
			}
			return names
		}); err != nil {
		return fmt.Errorf("cannot index VolumeTierPolicy tiers: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&tierdv1.VolumeTierPolicy{}).
		Watches(&storagev1.VolumeAttributesClass{}, handler.EnqueueRequestsFromMapFunc(r.policiesForVAC)).
		Named("volumetierpolicy").
		Complete(r)
}
