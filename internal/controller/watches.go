package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	claimVolumeIndex    = "hornkeeper.pvc.volumeName"
	volumeHandleIndex   = "hornkeeper.pv.volumeHandle"
	claimBackupTargetIndex = "hornkeeper.pvc.backupTarget"
)

func ClaimVolumeIndex(obj client.Object) []string {
	pvc := obj.(*corev1.PersistentVolumeClaim)
	if pvc.Spec.VolumeName == "" {
		return nil
	}
	return []string{pvc.Spec.VolumeName}
}

func VolumeHandleIndex(obj client.Object) []string {
	pv := obj.(*corev1.PersistentVolume)
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != LonghornDriver || pv.Spec.CSI.VolumeHandle == "" {
		return nil
	}
	return []string{pv.Spec.CSI.VolumeHandle}
}

func ClaimBackupTargetIndex(obj client.Object) []string {
	pvc := obj.(*corev1.PersistentVolumeClaim)
	if value, exists := pvc.Labels[BackupTargetLabel]; exists && value != "" {
		return []string{value}
	}
	return nil
}

func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.PersistentVolumeClaim{}, claimVolumeIndex, ClaimVolumeIndex); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.PersistentVolume{}, volumeHandleIndex, VolumeHandleIndex); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1.PersistentVolumeClaim{}, claimBackupTargetIndex, ClaimBackupTargetIndex); err != nil {
		return err
	}
	for _, obj := range []client.Object{Resource(VolumeGVK), Resource(BackupTargetGVK)} {
		if _, err := mgr.GetCache().GetInformer(ctx, obj, cache.BlockUntilSynced(false)); err != nil {
			return err
		}
	}
	optIn := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return e.Object.GetLabels()[EnabledLabel] == "true" },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetLabels()[EnabledLabel] == "true" || e.ObjectNew.GetLabels()[EnabledLabel] == "true"
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return e.Object.GetLabels()[EnabledLabel] == "true" },
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("hornkeeper").
		For(&corev1.PersistentVolumeClaim{}, builder.WithPredicates(optIn)).
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(r.ClaimsForPV)).
		Watches(Resource(VolumeGVK), handler.EnqueueRequestsFromMapFunc(r.ClaimsForVolume), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(Resource(BackupTargetGVK), handler.EnqueueRequestsFromMapFunc(r.ClaimsForTarget)).
		Complete(r)
}

func (r *Reconciler) ClaimsForPV(ctx context.Context, obj client.Object) []reconcile.Request {
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.Client.List(ctx, claims, client.MatchingFields{claimVolumeIndex: obj.GetName()}, client.MatchingLabels{EnabledLabel: "true"}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot map PV event to PVCs", "pv", obj.GetName())
		return nil
	}
	return claimRequests(claims)
}

func (r *Reconciler) ClaimsForVolume(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.Config.LonghornNamespace {
		return nil
	}
	volumes := &corev1.PersistentVolumeList{}
	if err := r.Client.List(ctx, volumes, client.MatchingFields{volumeHandleIndex: obj.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot map Longhorn event to PVs", "volume", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range volumes.Items {
		requests = append(requests, r.ClaimsForPV(ctx, &volumes.Items[i])...)
	}
	return requests
}

func (r *Reconciler) ClaimsForTarget(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.Config.LonghornNamespace {
		return nil
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.Client.List(ctx, claims, client.MatchingFields{claimBackupTargetIndex: obj.GetName()}, client.MatchingLabels{EnabledLabel: "true"}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot map backup target event to PVCs", "target", obj.GetName())
		return nil
	}
	return claimRequests(claims)
}

func claimRequests(claims *corev1.PersistentVolumeClaimList) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(claims.Items))
	for i := range claims.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&claims.Items[i])})
	}
	return requests
}
