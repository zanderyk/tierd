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
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
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
const (
	ConditionVTSManaged        = "Managed"
	ConditionVTSModifying      = "Modifying"
	ConditionVTSManualOverride = "ManualOverride"
)

const (
	ConditionReasonPolicyMatched  = "PolicyMatched"
	ConditionReasonInvalidPVC     = "InvalidPVC"
	ConditionReasonMissingPolicy  = "MissingPolicy"
	ConditionReasonAwaitingSample = "AwaitingSample"
	// ConditionReasonTierNotInPolicy: the PVC's VolumeAttributesClass isn't
	// one of the policy's tiers.
	ConditionReasonTierNotInPolicy = "TierNotInPolicy"
	// ConditionReasonCannotAdopt: the PVC has no VolumeAttributesClass and
	// tierd can't pick a starting tier for it.
	ConditionReasonCannotAdopt    = "CannotAdopt"
	ConditionReasonManualOverride = "VolumeAttributesClassManualOverride"
)

// errPVCNotBound means the PVC exists but has not bound to a volume yet.
var errPVCNotBound = errors.New("PVC is not bound")

// VolumeTierStateReconciler reconciles a VolumeTierState object
type VolumeTierStateReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierstates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierstates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumetierstates/finalizers,verbs=update
// +kubebuilder:rbac:groups=tierd.zanderyk.github.io,resources=volumeiosamples,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=patch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=volumeattributesclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

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

	if err := r.syncOverride(ctx, vts, pvc); err != nil {
		return ctrl.Result{}, err
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
			return ctrl.Result{}, err
		}

		setManaged(vts, metav1.ConditionFalse, ConditionReasonAwaitingSample,
			fmt.Sprintf("Waiting for the node agent's VolumeIOSample (policy %s)", policyName))
		return ctrl.Result{}, r.patchStatus(ctx, vts, base)
	}

	// Agent can't measure this type of volume.
	if meta.IsStatusConditionFalse(vioSample.Status.Conditions, tierdv1.VolumeIOSampleMeasured) {
		cond := meta.FindStatusCondition(vioSample.Status.Conditions, tierdv1.VolumeIOSampleMeasured)
		setManaged(vts, metav1.ConditionFalse, cond.Reason, cond.Message)
		return ctrl.Result{}, r.patchStatus(ctx, vts, base)
	}

	if err := r.pruneInfeasible(ctx, &vts.Status, pvc); err != nil {
		return ctrl.Result{}, err
	}

	requeue, managed, err := r.recommendTier(ctx, vtp, vts, pvc, vioSample)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !managed {
		return ctrl.Result{}, r.patchStatus(ctx, vts, base)
	}

	if shouldAct(vtp, vts, pvc) {
		pvcBase := pvc.DeepCopy()
		setTier(pvc, vts.Status.RecommendedTier)
		if err := r.Patch(ctx, pvc, client.MergeFrom(pvcBase)); err != nil {
			return ctrl.Result{}, err
		}

		now := metav1.Now()
		vts.Status.LastAppliedTier = vts.Status.RecommendedTier
		vts.Status.LastModified = &now
		// Start the stopwatch fresh on the new tier
		vts.Status.PendingDirection = tierdv1.ScaleDirectionNone
		vts.Status.PendingSince = nil
		if vts.Status.CurrentTier == "" {
			r.Recorder.Eventf(pvc, vts, corev1.EventTypeNormal, "TierChanged", "AdoptTier", "Set tier %s (policy: %s): %s", vts.Status.RecommendedTier, vtp.GetName(), vts.Status.Reason)
		} else {
			r.Recorder.Eventf(pvc, vts, corev1.EventTypeNormal, "TierChanged", "ChangeTier", "Changed tier from %s to %s (policy: %s): %s", vts.Status.CurrentTier, vts.Status.RecommendedTier, vtp.GetName(), vts.Status.Reason)
		}
	}

	if err := r.checkModify(ctx, vtp, vts, pvc); err != nil {
		return ctrl.Result{}, err
	}

	setManaged(vts, metav1.ConditionTrue, ConditionReasonPolicyMatched, fmt.Sprintf("Managed by VolumeTierPolicy %s", vts.Status.PolicyRef))

	if err := r.patchStatus(ctx, vts, base); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeue}, nil
}

// recommendTier sets the state's recommended tier. A PVC with no
// VolumeAttributesClass is adopted onto the tier that fits its load; one on
// a tier is evaluated against the policy's triggers. managed is false, with
// the Managed condition set, when the PVC can't be tiered.
func (r *VolumeTierStateReconciler) recommendTier(
	ctx context.Context,
	vtp *tierdv1.VolumeTierPolicy,
	vts *tierdv1.VolumeTierState,
	pvc *corev1.PersistentVolumeClaim,
	sample *tierdv1.VolumeIOSample,
) (requeue time.Duration, managed bool, err error) {
	if vts.Status.CurrentTier != "" {
		if tierIndex(vtp.Spec, vts.Status.CurrentTier) < 0 {
			setManaged(vts, metav1.ConditionFalse, ConditionReasonTierNotInPolicy,
				fmt.Sprintf("VolumeAttributesClass %s is not one of the policy's tiers", vts.Status.CurrentTier))
			return 0, false, nil
		}
		direction, why := r.Evaluate(vtp, vts.Status.CurrentTier, sample)
		return recommend(&vts.Status, vtp.Spec, direction, why, time.Now()), true, nil
	}

	// Adopt: only onto classes the PVC's own driver can apply.
	msg, err := r.driverMismatch(ctx, vtp, pvc)
	if err != nil {
		return 0, false, err
	}
	if msg == "" {
		tier, why, ok := pickTier(vtp.Spec, sample.Status)
		if ok {
			vts.Status.RecommendedTier, vts.Status.Reason = tier, why
			return 0, true, nil
		}
		msg = "No tier declares ceilings, so the starting tier can't be chosen from the load; set the PVC's VolumeAttributesClass to one of the policy's tiers"
	}
	setManaged(vts, metav1.ConditionFalse, ConditionReasonCannotAdopt, msg)
	return 0, false, nil
}

// driverMismatch explains why the policy's tiers can't apply to pvc's
// volume, or returns "" if they can. The policy's tiers share one driver.
func (r *VolumeTierStateReconciler) driverMismatch(ctx context.Context, vtp *tierdv1.VolumeTierPolicy, pvc *corev1.PersistentVolumeClaim) (string, error) {
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return "", fmt.Errorf("cannot get PersistentVolume %s: %w", pvc.Spec.VolumeName, err)
	}
	if pv.Spec.CSI == nil {
		return fmt.Sprintf("PersistentVolume %s is not a CSI volume", pv.Name), nil
	}
	vacName := vtp.Spec.Tiers[0].VACName
	vac := &storagev1.VolumeAttributesClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: vacName}, vac); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("VolumeAttributesClass %s not found", vacName), nil
		}
		return "", err
	}
	if vac.DriverName != pv.Spec.CSI.Driver {
		return fmt.Sprintf("PVC uses driver %s but the policy's tiers are for %s", pv.Spec.CSI.Driver, vac.DriverName), nil
	}
	return "", nil
}

// checkModify sets the Modifying condition from the PVC's modify status. On
// a failure it records the tier as infeasible and, in Act mode, rolls back.
func (r *VolumeTierStateReconciler) checkModify(ctx context.Context, vtp *tierdv1.VolumeTierPolicy,
	vts *tierdv1.VolumeTierState, pvc *corev1.PersistentVolumeClaim) error {
	mvs := pvc.Status.ModifyVolumeStatus
	if mvs == nil {
		setCondition(vts, ConditionVTSModifying, metav1.ConditionFalse, "Idle", "")
		return nil
	}

	// Some drivers (Azure) never report Infeasible; they keep the PVC
	// InProgress and set a ModifyVolumeError condition instead.
	msg, failed := hasModifyError(pvc)

	failed = failed && mvs.TargetVolumeAttributesClassName != vts.Status.CurrentTier
	switch {
	case mvs.Status == corev1.PersistentVolumeClaimModifyVolumeInfeasible || failed:
		if msg == "" {
			msg = "driver rejected the change"
		}
		vac := &storagev1.VolumeAttributesClass{}
		if err := r.Get(ctx, client.ObjectKey{Name: mvs.TargetVolumeAttributesClassName}, vac); client.IgnoreNotFound(err) != nil {
			return err
		}
		markInfeasible(&vts.Status, mvs.TargetVolumeAttributesClassName, msg,
			vac.UID, pvc.Status.Capacity.Storage(), time.Now())
		setCondition(
			vts,
			ConditionVTSModifying,
			metav1.ConditionFalse,
			"Failed",
			"Failed to move to "+mvs.TargetVolumeAttributesClassName,
		)

		// Rollback VolumeAttributesClass in the event the volume can't be migrated.
		// Don't undo a tier the user set by hand, even if it failed. An
		// adopted PVC (no tier before) can't be rolled back: Kubernetes
		// won't unset volumeAttributesClassName.
		if vtp.Spec.Mode == tierdv1.VolumeTierPolicyModeAct &&
			vts.Status.CurrentTier != "" &&
			!meta.IsStatusConditionTrue(vts.Status.Conditions, ConditionVTSManualOverride) &&
			ptr.Deref(pvc.Spec.VolumeAttributesClassName, "") != vts.Status.CurrentTier {
			pvcBase := pvc.DeepCopy()
			setTier(pvc, vts.Status.CurrentTier)
			if err := r.Patch(ctx, pvc, client.MergeFrom(pvcBase)); err != nil {
				return err
			}
			vts.Status.LastAppliedTier = vts.Status.CurrentTier
			r.Recorder.Eventf(pvc, vts, corev1.EventTypeWarning, "TierChangeFailed", "RollbackTier",
				"Could not move to %s; rolled back to %s", mvs.TargetVolumeAttributesClassName, vts.Status.CurrentTier)
		}
	case mvs.Status == corev1.PersistentVolumeClaimModifyVolumeInProgress,
		mvs.Status == corev1.PersistentVolumeClaimModifyVolumePending:
		// waiting
		target := mvs.TargetVolumeAttributesClassName
		if target == "" {
			target = ptr.Deref(pvc.Spec.VolumeAttributesClassName, "")
		}
		setCondition(vts, ConditionVTSModifying, metav1.ConditionTrue, string(mvs.Status), "Moving to "+target)
	}
	return nil
}

// syncOverride sets the ManualOverride condition from the PVC's annotation.
// It adds the annotation when someone changes the tier by hand, and resumes
// from the PVC's current tier once the annotation is removed.
func (r *VolumeTierStateReconciler) syncOverride(ctx context.Context,
	vts *tierdv1.VolumeTierState, pvc *corev1.PersistentVolumeClaim) error {
	specVAC := ptr.Deref(pvc.Spec.VolumeAttributesClassName, "")
	_, annotated := pvc.Annotations[ManualOverrideAnnotation]
	wasOverridden := meta.IsStatusConditionTrue(vts.Status.Conditions, ConditionVTSManualOverride)
	paused := fmt.Sprintf("tierd is paused; remove annotation %s to resume", ManualOverrideAnnotation)

	switch {
	case annotated:
		setCondition(vts, ConditionVTSManualOverride, metav1.ConditionTrue, "Annotated", paused)
	case wasOverridden:
		// The user removed the annotation: take the current tier as tierd's own
		// and restart the `for` window, so load seen while paused can't act
		// the moment tierd resumes.
		if pvc.Annotations[AppliedTierAnnotation] != specVAC {
			pvcBase := pvc.DeepCopy()
			metav1.SetMetaDataAnnotation(&pvc.ObjectMeta, AppliedTierAnnotation, specVAC)
			if err := r.Patch(ctx, pvc, client.MergeFrom(pvcBase)); err != nil {
				return err
			}
		}
		vts.Status.LastAppliedTier = specVAC
		vts.Status.PendingDirection = tierdv1.ScaleDirectionNone
		vts.Status.PendingSince = nil
		setCondition(vts, ConditionVTSManualOverride, metav1.ConditionFalse, "Resumed", "")
	case changedByHand(pvc, vts.Status, specVAC):
		pvcBase := pvc.DeepCopy()
		metav1.SetMetaDataAnnotation(&pvc.ObjectMeta, ManualOverrideAnnotation, "true")
		if err := r.Patch(ctx, pvc, client.MergeFrom(pvcBase)); err != nil {
			return err
		}
		r.Recorder.Eventf(pvc, vts, corev1.EventTypeNormal, "ManualOverride", "PauseTiering",
			"Tier changed by hand to %s; %s", specVAC, paused)
		setCondition(vts, ConditionVTSManualOverride, metav1.ConditionTrue, "ChangedByHand", paused)
	default:
		setCondition(vts, ConditionVTSManualOverride, metav1.ConditionFalse, "NoOverride", "")
	}
	return nil
}

// changedByHand is true if the PVC's spec tier is not the one tierd last
// set, or, if tierd never set one, not the PVC's current tier.
//
// What tierd set is read from the PVC's applied-tier annotation, written in
// the same patch as the tier. The state's LastAppliedTier can't be trusted
// here: the PVC patch triggers the next reconcile, which may still read the
// state from a cache without it. LastAppliedTier is only a fallback for PVCs
// tierd changed before the annotation existed.
func changedByHand(pvc *corev1.PersistentVolumeClaim, st tierdv1.VolumeTierStateStatus, specVAC string) bool {
	want := cmp.Or(pvc.Annotations[AppliedTierAnnotation], st.LastAppliedTier, st.CurrentTier)
	return want != "" && specVAC != want
}

// setTier sets the PVC's tier and records it as tierd's own change.
func setTier(pvc *corev1.PersistentVolumeClaim, tier string) {
	pvc.Spec.VolumeAttributesClassName = new(tier)
	metav1.SetMetaDataAnnotation(&pvc.ObjectMeta, AppliedTierAnnotation, tier)
}

const (
	infeasibleBackoff    = 30 * time.Minute
	infeasibleBackoffMax = 6 * time.Hour
)

// markInfeasible records that moving to tier failed. vacUID and capacity
// let pruneInfeasible drop the entry once the class or PVC size changes.
func markInfeasible(st *tierdv1.VolumeTierStateStatus, tier, message string,
	vacUID types.UID, capacity *resource.Quantity, now time.Time) {

	i := slices.IndexFunc(st.InfeasibleTiers, func(t tierdv1.InfeasibleTier) bool { return t.Name == tier })
	if i < 0 {
		st.InfeasibleTiers = append(st.InfeasibleTiers, tierdv1.InfeasibleTier{Name: tier})
		i = len(st.InfeasibleTiers) - 1
	}

	e := &st.InfeasibleTiers[i]

	// Count each attempt once. Skip if already recorded since the last change.
	if e.Attempts > 0 && (st.LastModified == nil || e.Since.After(st.LastModified.Time)) {
		return
	}

	e.Attempts++
	e.Since = metav1.NewTime(now)
	wait := min(infeasibleBackoff*time.Duration(e.Attempts), infeasibleBackoffMax)
	e.RetryAfter = metav1.NewTime(now.Add(wait))
	e.Message = message
	e.VACUID = vacUID
	e.Capacity = capacity
}

// pruneInfeasible drops entries that may now succeed: the class was deleted
// or recreated, or the PVC has grown.
func (r *VolumeTierStateReconciler) pruneInfeasible(
	ctx context.Context,
	st *tierdv1.VolumeTierStateStatus,
	pvc *corev1.PersistentVolumeClaim,
) error {
	size := pvc.Status.Capacity.Storage()
	var keep []tierdv1.InfeasibleTier
	for _, e := range st.InfeasibleTiers {
		vac := &storagev1.VolumeAttributesClass{}
		err := r.Get(ctx, client.ObjectKey{Name: e.Name}, vac)
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		switch {
		case err != nil: // class deleted
		case e.VACUID != "" && e.VACUID != vac.UID: // class recreated
		case e.Capacity != nil && size.Cmp(*e.Capacity) > 0: // PVC grew
		default:
			keep = append(keep, e)
		}
	}
	st.InfeasibleTiers = keep
	return nil
}

// backoffUntil is when tier's infeasible backoff ends; ok is false if the
// tier never failed.
func backoffUntil(st tierdv1.VolumeTierStateStatus, tier string) (until time.Time, ok bool) {
	i := slices.IndexFunc(st.InfeasibleTiers, func(t tierdv1.InfeasibleTier) bool { return t.Name == tier })
	if i < 0 {
		return time.Time{}, false
	}
	return st.InfeasibleTiers[i].RetryAfter.Time, true
}

// hasModifyError returns the driver's error if the last modify attempt failed.
func hasModifyError(pvc *corev1.PersistentVolumeClaim) (string, bool) {
	for _, c := range pvc.Status.Conditions {
		if c.Type ==
			corev1.PersistentVolumeClaimVolumeModifyVolumeError && c.Status ==
			corev1.ConditionTrue {
			return c.Message, true
		}
	}
	return "", false
}

func setCondition(vts *tierdv1.VolumeTierState, typ string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(
		&vts.Status.Conditions,
		metav1.Condition{
			Type:               typ,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: vts.Generation,
		},
	)
}

// shouldAct reports whether the controller should move the PVC to the
// recommended tier now.
func shouldAct(vtp *tierdv1.VolumeTierPolicy, vts *tierdv1.VolumeTierState, pvc *corev1.PersistentVolumeClaim) bool {
	st := vts.Status
	switch {
	case vtp.Spec.Mode != tierdv1.VolumeTierPolicyModeAct:
		return false
	case meta.IsStatusConditionTrue(st.Conditions, ConditionVTSManualOverride):
		return false // the user picked the tier; leave it
	case st.RecommendedTier == "" || st.RecommendedTier == st.CurrentTier:
		return false
	case ptr.Deref(pvc.Spec.VolumeAttributesClassName, "") != st.CurrentTier:
		return false // change is already in flight
	}
	// Wait out the backoff on a tier the driver rejected.
	if until, ok := backoffUntil(st, st.RecommendedTier); ok && time.Now().Before(until) {
		return false
	}
	// TODO: do we need a cooldown to check storage provider's QoS implementations?
	return true
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
