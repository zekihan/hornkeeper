package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/zekihan/hornkeeper/internal/config"
)

// backupTargetCheckTimeout bounds the startup backup-target lookup so it
// cannot block reconciliation for long.
const backupTargetCheckTimeout = 5 * time.Second

type Reconciler struct {
	Client  client.Client
	Reader  client.Reader
	Config  config.Config
	Metrics *Metrics
}

func (rec *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := rec.Reader.Get(ctx, req.NamespacedName, pvc); err != nil {
		if err := client.IgnoreNotFound(err); err != nil {
			rec.Metrics.Reconciles.WithLabelValues("error").Inc()
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if !enabled(pvc) {
		if unrecognizedOptIn(pvc) {
			slog.New(logr.ToSlogHandler(ctrl.LoggerFrom(ctx))).
				WarnContext(ctx, "Ignoring claim with unrecognized opt-in value; use \"true\" or \"false\"",
					slog.String("label", EnabledLabel),
					slog.String("value", pvc.Labels[EnabledLabel]))
		}
		rec.Metrics.Reconciles.WithLabelValues("ignored").Inc()
		return ctrl.Result{}, nil
	}
	// Each field has its own patch so an admission rejection cannot block the others.
	var errs []error
	for _, field := range volumeFields {
		if _, exists := pvc.Annotations[field.annotation]; field.optional() && !exists {
			continue
		}
		errs = append(errs, rec.reconcileField(ctx, req.NamespacedName, field))
	}
	if err := errors.Join(errs...); err != nil {
		rec.Metrics.Reconciles.WithLabelValues("error").Inc()
		return ctrl.Result{}, err
	}
	rec.Metrics.Reconciles.WithLabelValues("enabled").Inc()
	return ctrl.Result{RequeueAfter: rec.Config.ResyncInterval}, nil
}

func enabled(pvc *corev1.PersistentVolumeClaim) bool {
	return pvc.Labels[EnabledLabel] == enabledLabelValue && pvc.DeletionTimestamp.IsZero()
}

// unrecognizedOptIn reports an enabled label that is neither "true" nor "false",
// such as "enabled" copied from Longhorn's own PVC labels.
func unrecognizedOptIn(obj client.Object) bool {
	value, exists := obj.GetLabels()[EnabledLabel]
	return exists && value != enabledLabelValue && value != disabledLabelValue
}

func (rec *Reconciler) resolveBinding(
	ctx context.Context,
	key client.ObjectKey,
) (*corev1.PersistentVolumeClaim, string, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := rec.Reader.Get(ctx, key, pvc); err != nil {
		return nil, "", client.IgnoreNotFound(err)
	}
	if !enabled(pvc) || pvc.Spec.VolumeName == "" || pvc.Status.Phase != corev1.ClaimBound {
		return pvc, "", nil
	}
	pv := &corev1.PersistentVolume{}
	if err := rec.Reader.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return pvc, "", client.IgnoreNotFound(err)
	}
	handle, ok := volumeHandle(pvc, pv)
	if !ok {
		ctrl.LoggerFrom(ctx).Info("Waiting for matching bound PV claim reference", "pv", pv.Name)

		return pvc, "", nil
	}

	return pvc, handle, nil
}

// volumeHandle returns the Longhorn volume handle for a PV whose claim reference
// points at pvc and whose CSI volume handle is set.
func volumeHandle(
	pvc *corev1.PersistentVolumeClaim,
	pv *corev1.PersistentVolume,
) (string, bool) {
	ref := pv.Spec.ClaimRef
	if !pv.DeletionTimestamp.IsZero() || pv.Status.Phase != corev1.VolumeBound || ref == nil ||
		ref.Namespace != pvc.Namespace || ref.Name != pvc.Name || ref.UID != pvc.UID {
		return "", false
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != LonghornDriver ||
		pv.Spec.CSI.VolumeHandle == "" {
		return "", false
	}

	return pv.Spec.CSI.VolumeHandle, true
}

// errVolumeNotReady reports that the volume is not there yet (not found or deleting).
var errVolumeNotReady = errors.New("volume is not ready")

func (rec *Reconciler) readVolume(
	ctx context.Context,
	handle string,
) (*unstructured.Unstructured, error) {
	volume := Resource(VolumeGVK)
	if err := rec.Reader.Get(
		ctx,
		client.ObjectKey{Namespace: rec.Config.LonghornNamespace, Name: handle},
		volume,
	); err != nil {
		if isLonghornObjectNotFound(err, "volumes", handle) {
			return nil, errVolumeNotReady
		}
		return nil, err
	}
	if !volume.GetDeletionTimestamp().IsZero() {
		return nil, errVolumeNotReady
	}
	return volume, nil
}

// Metric result labels for field reconciliation.
const (
	resultWaiting   = "waiting"
	resultUnmanaged = "unmanaged"
	resultInvalid   = "invalid"
	resultUnchanged = "unchanged"
	resultDryRun    = "dry-run"
	resultPatched   = "patched"
	resultError     = "error"
)

func (rec *Reconciler) reconcileField(
	ctx context.Context,
	key client.ObjectKey,
	fieldDef volumeField,
) error {
	field := fieldDef.name
	result := resultWaiting
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		outcome, err := rec.attemptPatch(ctx, key, fieldDef)
		result = outcome

		return err
	})
	if err != nil {
		result = resultError
	}
	rec.Metrics.Fields.WithLabelValues(field, result).Inc()
	if err != nil {
		return fmt.Errorf("reconcile %s: %w", field, err)
	}
	return nil
}

// attemptPatch applies one field patch attempt, returning the metric result
// label to record for the attempt.
func (rec *Reconciler) attemptPatch(
	ctx context.Context,
	key client.ObjectKey,
	fieldDef volumeField,
) (string, error) {
	field := fieldDef.name
	// Direct API reads on every attempt re-check opt-in, binding and resourceVersion.
	pvc, handle, err := rec.resolveBinding(ctx, key)
	if err != nil || handle == "" {
		return resultWaiting, err
	}
	volume, err := rec.readVolume(ctx, handle)
	if errors.Is(err, errVolumeNotReady) {
		return resultWaiting, nil
	}
	if err != nil {
		return resultWaiting, err
	}
	log := ctrl.LoggerFrom(ctx).WithValues("volume", volume.GetName(), "field", field)
	current, desired, result, err := rec.readDesired(ctx, key, pvc, handle, volume, fieldDef, log)
	if err != nil {
		return resultWaiting, err
	}
	if result != "" {
		return result, nil
	}

	return rec.applyPatch(ctx, volume, field, current, desired, log)
}

// readDesired resolves the desired value and the current value for a field.
// A non-empty result means the attempt outcome is already decided.
func (rec *Reconciler) readDesired(
	ctx context.Context,
	key client.ObjectKey,
	pvc *corev1.PersistentVolumeClaim,
	handle string,
	volume *unstructured.Unstructured,
	fieldDef volumeField,
	log logr.Logger,
) (any, any, string, error) {
	field := fieldDef.name
	desired, err := rec.desired(ctx, pvc, volume, fieldDef)
	if err != nil {
		if errors.Is(err, errUnmanaged) {
			return nil, nil, resultUnmanaged, nil
		}
		if isInvalid(err) {
			log.Error(err, "Leaving field unchanged")

			return nil, nil, resultInvalid, nil
		}

		return nil, nil, "", err
	}
	current, found, err := unstructured.NestedFieldNoCopy(volume.Object, "spec", field)
	if err != nil {
		return nil, nil, "", err
	}
	if !found && fieldDef.zeroWhenAbsent {
		current = int64(0)
	}
	if reflect.DeepEqual(current, desired) {
		return current, desired, resultUnchanged, nil
	}
	// The volume and backup target reads can outlast a PV binding or a claim policy change.
	latestClaim, latestHandle, err := rec.resolveBinding(ctx, key)
	if err != nil || latestHandle != handle {
		return current, desired, resultWaiting, err
	}
	if latestClaim.UID != pvc.UID || latestClaim.ResourceVersion != pvc.ResourceVersion {
		return current, desired, resultWaiting, nil
	}

	return current, desired, "", nil
}

// applyPatch records a dry run or applies the patch for a changed field.
func (rec *Reconciler) applyPatch(
	ctx context.Context,
	volume *unstructured.Unstructured,
	field string,
	current, desired any,
	log logr.Logger,
) (string, error) {
	if rec.Config.DryRun {
		log.Info(
			"Dry run: would update desired volume setting",
			"previous",
			current,
			"desired",
			desired,
		)

		return resultDryRun, nil
	}
	base := volume.DeepCopy()
	if err := unstructured.SetNestedField(volume.Object, desired, "spec", field); err != nil {
		return resultWaiting, err
	}
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := rec.Client.Patch(ctx, volume, patch); err != nil {
		log.Error(err, "Volume patch rejected; field remains unchanged", "desired", desired)

		return resultWaiting, err
	}
	log.Info("Updated desired volume setting", "previous", current, "desired", desired)

	return resultPatched, nil
}

type invalidError struct{ error }

func invalid(err error) error { return invalidError{err} }
func isInvalid(err error) bool {
	var value invalidError
	return errors.As(err, &value)
}

func (rec *Reconciler) desired(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
	volume *unstructured.Unstructured,
	fieldDef volumeField,
) (any, error) {
	switch fieldDef.name {
	case "backupTargetName":
		return rec.desiredBackupTarget(ctx, pvc)
	case "numberOfReplicas":
		return rec.desiredReplicas(pvc, volume)
	}
	if !fieldDef.optional() {
		return nil, fmt.Errorf("unknown field %q", fieldDef.name)
	}
	value, exists := pvc.Annotations[fieldDef.annotation]
	if !exists {
		return nil, errUnmanaged
	}
	parsed, err := fieldDef.parse(value)
	if err != nil {
		return nil, invalid(fmt.Errorf("%s=%q: %w", fieldDef.annotation, value, err))
	}
	if fieldDef.check != nil {
		if err := fieldDef.check(volume, parsed); err != nil {
			return nil, err
		}
	}
	if fieldDef.annotation == DataLocalityAnnotation && parsed == strictLocal {
		// Entering strict-local would prevent a valid increase in replicas.
		count, err := rec.desiredReplicas(pvc, volume)
		if err != nil && !isInvalid(err) {
			return nil, err
		}
		if err == nil && count > 1 {
			return nil, invalid(
				fmt.Errorf(
					"dataLocality=strict-local conflicts with desired replica count %d, which must be 1",
					count,
				),
			)
		}
	}
	return parsed, nil
}

func (rec *Reconciler) desiredBackupTarget(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
) (string, error) {
	name := rec.Config.BackupTarget
	if value, exists := pvc.Annotations[BackupTargetAnnotation]; exists {
		name = value
	}
	if err := config.ValidateBackupTarget(name); err != nil {
		return "", invalid(fmt.Errorf("%s=%q: %w", BackupTargetAnnotation, name, err))
	}
	// Use a short timeout for backup target validation to avoid blocking reconciliation.
	checkCtx, cancel := context.WithTimeout(ctx, backupTargetCheckTimeout)
	defer cancel()
	if err := CheckBackupTarget(
		checkCtx,
		rec.Reader,
		rec.Config.LonghornNamespace,
		name,
	); err != nil {
		if isInvalid(err) || isLonghornObjectNotFound(err, "backuptargets", name) {
			return "", invalid(err)
		}
		return "", err
	}
	return name, nil
}

func (rec *Reconciler) desiredReplicas(
	pvc *corev1.PersistentVolumeClaim,
	volume *unstructured.Unstructured,
) (int64, error) {
	count := rec.Config.Replicas
	if value, exists := pvc.Annotations[ReplicasAnnotation]; exists {
		parsed, err := parsePositiveInt(value)
		if err != nil {
			return 0, invalid(fmt.Errorf("%s=%q: %w", ReplicasAnnotation, value, err))
		}
		count = parsed
	}
	if err := config.ValidateReplicas(count); err != nil {
		return 0, invalid(err)
	}
	if err := checkReplicaLayout(volume, count); err != nil {
		return 0, err
	}

	return int64(count), nil
}

// parsePositiveInt accepts only decimal digits; signs and whitespace are rejected.
func parsePositiveInt(value string) (int, error) {
	if value == "" ||
		strings.IndexFunc(value, func(c rune) bool { return c < '0' || c > '9' }) >= 0 {
		return 0, errors.New("expected an integer from 1 to 20")
	}

	return strconv.Atoi(value)
}

// checkReplicaLayout validates replica-count restrictions of the volume layout.
func checkReplicaLayout(volume *unstructured.Unstructured, count int) error {
	locality, err := volumeString(volume, "spec", "dataLocality")
	if err != nil {
		return err
	}
	if locality == strictLocal && count != 1 {
		return invalid(
			fmt.Errorf(
				"replicas=%d conflicts with dataLocality=strict-local, which requires 1",
				count,
			),
		)
	}
	layout, err := volumeString(volume, "spec", "dataLayout", "type")
	if err != nil {
		return err
	}
	if layout == "sharded" && count != 1 {
		return invalid(
			fmt.Errorf("replicas=%d conflicts with sharded data layout, which requires 1", count),
		)
	}
	if volume.GetLabels()[legacyLinkedCloneLabel] == legacyCloneValue {
		current, err := volumeInt64(volume, "spec", "numberOfReplicas")
		if err != nil {
			return err
		}
		if current != int64(count) {
			return invalid(
				errors.New("replica count is immutable on legacy linked-clone volumes"),
			)
		}
	}

	return nil
}
