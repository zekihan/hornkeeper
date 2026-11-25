package controller

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kerror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kschema "k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	keyPrefix              = "hornkeeper.noqer.com/"
	EnabledLabel           = keyPrefix + "enabled"
	BackupTargetAnnotation = keyPrefix + "backup-target"
	ReplicasAnnotation     = keyPrefix + "replicas"
	LonghornDriver         = "driver.longhorn.io"
	longhornGroup          = "longhorn.io"
	legacyLinkedCloneLabel = "longhorn.io/legacy-linked-clone"

	// Recognized values of the enabled label.
	enabledLabelValue  = "true"
	disabledLabelValue = "false"

	// Longhorn sets the legacy-linked-clone label to "true" on linked clones.
	legacyCloneValue = "true"
)

var (
	VolumeGVK = kschema.GroupVersionKind{
		Group:   longhornGroup,
		Version: "v1beta2",
		Kind:    "Volume",
	}
	BackupTargetGVK = kschema.GroupVersionKind{
		Group:   longhornGroup,
		Version: "v1beta2",
		Kind:    "BackupTarget",
	}
)

func Resource(gvk kschema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

func isLonghornObjectNotFound(err error, resource, name string) bool {
	// The client attaches request details to 404s from unserved HTTP routes.
	var status kerror.APIStatus
	if !errors.As(err, &status) || kerror.IsUnexpectedServerError(err) {
		return false
	}
	response := status.Status()
	details := response.Details
	return response.Reason == metav1.StatusReasonNotFound && details != nil &&
		details.Group == longhornGroup && details.Kind == resource && details.Name == name
}

func CheckBackupTarget(ctx context.Context, reader client.Reader, namespace, name string) error {
	target := Resource(BackupTargetGVK)
	if err := reader.Get(
		ctx,
		client.ObjectKey{Namespace: namespace, Name: name},
		target,
	); err != nil {
		return fmt.Errorf("get backup target %s/%s: %w", namespace, name, err)
	}
	if !target.GetDeletionTimestamp().IsZero() {
		return invalid(fmt.Errorf("backup target %s/%s is being deleted", namespace, name))
	}
	return nil
}
