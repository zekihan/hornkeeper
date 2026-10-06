package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/zekihan/hornkeeper/internal/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Reconciler struct {
	Client  client.Client
	Reader  client.Reader
	Config  config.Config
	Metrics *Metrics
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Reader.Get(ctx, req.NamespacedName, pvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !enabled(pvc) {
		r.Metrics.Reconciles.WithLabelValues("ignored").Inc()
		return ctrl.Result{}, nil
	}
	// Each field has its own patch so an admission rejection cannot block the other.
	backupErr := r.reconcileField(ctx, req.NamespacedName, "backupTargetName")
	replicasErr := r.reconcileField(ctx, req.NamespacedName, "numberOfReplicas")
	if err := errors.Join(backupErr, replicasErr); err != nil {
		r.Metrics.Reconciles.WithLabelValues("error").Inc()
		return ctrl.Result{}, err
	}
	r.Metrics.Reconciles.WithLabelValues("enabled").Inc()
	return ctrl.Result{RequeueAfter: r.Config.ResyncInterval}, nil
}

func enabled(pvc *corev1.PersistentVolumeClaim) bool {
	return pvc.Labels[EnabledLabel] == "true" && pvc.DeletionTimestamp.IsZero()
}

func (r *Reconciler) resolve(ctx context.Context, key client.ObjectKey) (*corev1.PersistentVolumeClaim, *unstructured.Unstructured, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Reader.Get(ctx, key, pvc); err != nil {
		return nil, nil, client.IgnoreNotFound(err)
	}
	if !enabled(pvc) || pvc.Spec.VolumeName == "" || pvc.Status.Phase != corev1.ClaimBound {
		return pvc, nil, nil
	}
	pv := &corev1.PersistentVolume{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return pvc, nil, client.IgnoreNotFound(err)
	}
	ref := pv.Spec.ClaimRef
	if !pv.DeletionTimestamp.IsZero() || pv.Status.Phase != corev1.VolumeBound || ref == nil ||
		ref.Namespace != pvc.Namespace || ref.Name != pvc.Name || ref.UID != pvc.UID || pvc.UID == "" {
		ctrl.LoggerFrom(ctx).Info("Waiting for matching bound PV claim reference", "pv", pv.Name)
		return pvc, nil, nil
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != LonghornDriver || pv.Spec.CSI.VolumeHandle == "" {
		return pvc, nil, nil
	}
	volume := Resource(VolumeGVK)
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Config.LonghornNamespace, Name: pv.Spec.CSI.VolumeHandle}, volume); err != nil {
		return pvc, nil, client.IgnoreNotFound(err)
	}
	if !volume.GetDeletionTimestamp().IsZero() {
		return pvc, nil, nil
	}
	return pvc, volume, nil
}

func (r *Reconciler) reconcileField(ctx context.Context, key client.ObjectKey, field string) error {
	result := "waiting"
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Direct API reads on every attempt re-check opt-in, binding and resourceVersion.
		pvc, volume, err := r.resolve(ctx, key)
		if err != nil || volume == nil {
			return err
		}
		log := ctrl.LoggerFrom(ctx).WithValues("volume", volume.GetName(), "field", field)
		desired, err := r.desired(ctx, pvc, volume, field)
		if err != nil {
			if isInvalid(err) {
				result = "invalid"
				log.Error(err, "Leaving field unchanged")
				return nil
			}
			return err
		}
		current, _, err := unstructured.NestedFieldNoCopy(volume.Object, "spec", field)
		if err != nil {
			return err
		}
		if reflect.DeepEqual(current, desired) {
			result = "unchanged"
			return nil
		}
		base := volume.DeepCopy()
		if err := unstructured.SetNestedField(volume.Object, desired, "spec", field); err != nil {
			return err
		}
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		if err := r.Client.Patch(ctx, volume, patch); err != nil {
			log.Error(err, "Volume patch rejected; field remains unchanged", "desired", desired)
			return err
		}
		result = "patched"
		log.Info("Updated desired volume setting", "previous", current, "desired", desired)
		return nil
	})
	if err != nil {
		result = "error"
	}
	r.Metrics.Fields.WithLabelValues(field, result).Inc()
	if err != nil {
		return fmt.Errorf("reconcile %s: %w", field, err)
	}
	return nil
}

type invalidValue struct{ error }

func invalid(err error) error { return invalidValue{err} }
func isInvalid(err error) bool {
	var value invalidValue
	return errors.As(err, &value)
}

func (r *Reconciler) desired(ctx context.Context, pvc *corev1.PersistentVolumeClaim, volume *unstructured.Unstructured, field string) (any, error) {
	switch field {
	case "backupTargetName":
		return r.desiredBackupTarget(ctx, pvc)
	case "numberOfReplicas":
		return r.desiredReplicas(ctx, pvc, volume)
	default:
		return nil, fmt.Errorf("unknown field %q", field)
	}
}

func (r *Reconciler) desiredBackupTarget(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (string, error) {
	name := r.Config.BackupTarget
	if value, exists := pvc.Labels[BackupTargetLabel]; exists {
		name = value
	}
	if err := config.ValidateBackupTarget(name); err != nil {
		return "", invalid(fmt.Errorf("%s=%q: %w", BackupTargetLabel, name, err))
	}
	// Use a short timeout for backup target validation to avoid blocking reconciliation.
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := CheckBackupTarget(checkCtx, r.Reader, r.Config.LonghornNamespace, name); err != nil {
		if apierrors.IsNotFound(err) || isInvalid(err) {
			return "", invalid(err)
		}
		return "", err
	}
	return name, nil
}

func (r *Reconciler) desiredReplicas(ctx context.Context, pvc *corev1.PersistentVolumeClaim, volume *unstructured.Unstructured) (int64, error) {
	count := r.Config.Replicas
	if value, exists := pvc.Labels[ReplicasLabel]; exists {
		// Only decimal digits are accepted; signs and whitespace are not label values.
		if value == "" || strings.IndexFunc(value, func(c rune) bool { return c < '0' || c > '9' }) >= 0 {
			return 0, invalid(fmt.Errorf("%s=%q: expected an integer from 1 to 20", ReplicasLabel, value))
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, invalid(fmt.Errorf("%s=%q: %w", ReplicasLabel, value, err))
		}
		count = parsed
	}
	if err := config.ValidateReplicas(count); err != nil {
		return 0, invalid(err)
	}
	locality, _, err := unstructured.NestedString(volume.Object, "spec", "dataLocality")
	if err != nil {
		return 0, err
	}
	if locality == "strict-local" && count != 1 {
		return 0, invalid(fmt.Errorf("replicas=%d conflicts with dataLocality=strict-local, which requires 1", count))
	}
	layout, _, err := unstructured.NestedString(volume.Object, "spec", "dataLayout", "type")
	if err != nil {
		return 0, err
	}
	if layout == "sharded" && count != 1 {
		return 0, invalid(fmt.Errorf("replicas=%d conflicts with sharded data layout, which requires 1", count))
	}
	if volume.GetLabels()["longhorn.io/legacy-linked-clone"] == "true" {
		current, _, err := unstructured.NestedInt64(volume.Object, "spec", "numberOfReplicas")
		if err != nil {
			return 0, err
		}
		if current != int64(count) {
			return 0, invalid(fmt.Errorf("replica count is immutable on legacy linked-clone volumes"))
		}
	}
	return int64(count), nil
}
