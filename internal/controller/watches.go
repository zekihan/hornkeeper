package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	claimVolumeIndex       = "hornkeeper.pvc.volumeName"
	volumeHandleIndex      = "hornkeeper.pv.volumeHandle"
	claimBackupTargetIndex = "hornkeeper.pvc.backupTarget"
)

func ClaimVolumeIndex(obj client.Object) []string {
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok || pvc.Spec.VolumeName == "" {
		return nil
	}
	return []string{pvc.Spec.VolumeName}
}

func VolumeHandleIndex(obj client.Object) []string {
	pv, ok := obj.(*corev1.PersistentVolume)
	if !ok || pv.Spec.CSI == nil || pv.Spec.CSI.Driver != LonghornDriver ||
		pv.Spec.CSI.VolumeHandle == "" {
		return nil
	}
	return []string{pv.Spec.CSI.VolumeHandle}
}

func (rec *Reconciler) ClaimBackupTargetIndex(obj client.Object) []string {
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok {
		return nil
	}
	name := rec.Config.BackupTarget
	if value, exists := pvc.Annotations[BackupTargetAnnotation]; exists {
		// An explicitly empty annotation is invalid, not an inherited default.
		name = value
	}
	if name == "" {
		return nil
	}
	return []string{name}
}

func (rec *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().
		IndexField(ctx, &corev1.PersistentVolumeClaim{}, claimVolumeIndex, ClaimVolumeIndex); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().
		IndexField(ctx, &corev1.PersistentVolume{}, volumeHandleIndex, VolumeHandleIndex); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().
		IndexField(ctx, &corev1.PersistentVolumeClaim{}, claimBackupTargetIndex, rec.ClaimBackupTargetIndex); err != nil {
		return err
	}
	for _, obj := range []client.Object{Resource(VolumeGVK), Resource(BackupTargetGVK)} {
		if _, err := mgr.GetCache().
			GetInformer(ctx, obj, cache.BlockUntilSynced(false)); err != nil {
			return err
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("hornkeeper").
		For(&corev1.PersistentVolumeClaim{}, builder.WithPredicates(optIn)).
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(rec.ClaimsForPV)).
		Watches(Resource(VolumeGVK), handler.EnqueueRequestsFromMapFunc(rec.ClaimsForVolume), builder.WithPredicates(volumeChanges)).
		Watches(Resource(BackupTargetGVK), handler.EnqueueRequestsFromMapFunc(rec.ClaimsForTarget)).
		Complete(rec)
}

var volumeChanges = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldVolume, oldOK := e.ObjectOld.(*unstructured.Unstructured)
		newVolume, newOK := e.ObjectNew.(*unstructured.Unstructured)
		if !oldOK || !newOK || oldVolume == nil || newVolume == nil {
			return false
		}
		if oldVolume.GetGeneration() != newVolume.GetGeneration() {
			return true
		}
		// Replica and locality restrictions can change without a spec update.
		oldLegacyClone := oldVolume.GetLabels()[legacyLinkedCloneLabel] == legacyCloneValue
		newLegacyClone := newVolume.GetLabels()[legacyLinkedCloneLabel] == legacyCloneValue
		if oldLegacyClone != newLegacyClone {
			return true
		}
		oldState, err := volumeString(oldVolume, "status", "state")
		if err != nil {
			return true
		}
		newState, err := volumeString(newVolume, "status", "state")
		if err != nil {
			return true
		}

		return (oldState == "detached") != (newState == "detached")
	},
}

var optIn = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool { return optInEvent(e.Object) },
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldValue, oldExists := e.ObjectOld.GetLabels()[EnabledLabel]
		newValue := e.ObjectNew.GetLabels()[EnabledLabel]
		// Unrecognized values are reconciled only when they change, so the warning is not repeated
		// on every update.
		return oldValue == enabledLabelValue || newValue == enabledLabelValue ||
			(unrecognizedOptIn(e.ObjectNew) && (!oldExists || oldValue != newValue))
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return false },
	GenericFunc: func(e event.GenericEvent) bool { return optInEvent(e.Object) },
}

// optInEvent admits enabled claims and claims whose opt-in value needs a warning.
func optInEvent(obj client.Object) bool {
	return obj.GetLabels()[EnabledLabel] == enabledLabelValue || unrecognizedOptIn(obj)
}

func (rec *Reconciler) ClaimsForPV(ctx context.Context, obj client.Object) []reconcile.Request {
	claims := &corev1.PersistentVolumeClaimList{}
	if err := rec.Client.List(
		ctx,
		claims,
		client.MatchingFields{claimVolumeIndex: obj.GetName()},
		client.MatchingLabels{EnabledLabel: enabledLabelValue},
	); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot map PV event to PVCs", "pv", obj.GetName())
		return nil
	}
	return claimRequests(claims)
}

func (rec *Reconciler) ClaimsForVolume(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != rec.Config.LonghornNamespace {
		return nil
	}
	volumes := &corev1.PersistentVolumeList{}
	if err := rec.Client.List(
		ctx,
		volumes,
		client.MatchingFields{volumeHandleIndex: obj.GetName()},
	); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot map Longhorn event to PVs", "volume", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range volumes.Items {
		requests = append(requests, rec.ClaimsForPV(ctx, &volumes.Items[i])...)
	}
	return requests
}

func (rec *Reconciler) ClaimsForTarget(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != rec.Config.LonghornNamespace {
		return nil
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := rec.Client.List(
		ctx,
		claims,
		client.MatchingFields{claimBackupTargetIndex: obj.GetName()},
		client.MatchingLabels{EnabledLabel: enabledLabelValue},
	); err != nil {
		ctrl.LoggerFrom(ctx).
			Error(err, "Cannot map backup target event to PVCs", "target", obj.GetName())
		return nil
	}
	return claimRequests(claims)
}

func claimRequests(claims *corev1.PersistentVolumeClaimList) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(claims.Items))
	for i := range claims.Items {
		requests = append(
			requests,
			reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&claims.Items[i])},
		)
	}
	return requests
}
