package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	keyPrefix              = "hornkeeper.noqer.com/"
	EnabledLabel           = keyPrefix + "enabled"
	BackupTargetAnnotation = keyPrefix + "backup-target"
	ReplicasAnnotation     = keyPrefix + "replicas"
	LonghornDriver         = "driver.longhorn.io"
	longhornGroup          = "longhorn.io"
	legacyLinkedCloneLabel = "longhorn.io/legacy-linked-clone"
)

var (
	VolumeGVK       = schema.GroupVersionKind{Group: longhornGroup, Version: "v1beta2", Kind: "Volume"}
	BackupTargetGVK = schema.GroupVersionKind{Group: longhornGroup, Version: "v1beta2", Kind: "BackupTarget"}
)

func Resource(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

func isLonghornObjectNotFound(err error, resource, name string) bool {
	// The client attaches request details to 404s from unserved HTTP routes.
	var status apierrors.APIStatus
	if !errors.As(err, &status) || apierrors.IsUnexpectedServerError(err) {
		return false
	}
	response := status.Status()
	details := response.Details
	return response.Reason == metav1.StatusReasonNotFound && details != nil &&
		details.Group == longhornGroup && details.Kind == resource && details.Name == name
}

func CheckBackupTarget(ctx context.Context, reader client.Reader, namespace, name string) error {
	target := Resource(BackupTargetGVK)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, target); err != nil {
		return fmt.Errorf("get backup target %s/%s: %w", namespace, name, err)
	}
	if !target.GetDeletionTimestamp().IsZero() {
		return invalid(fmt.Errorf("backup target %s/%s is being deleted", namespace, name))
	}
	return nil
}
