package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	EnabledLabel      = "hornkeeper.noqer.com/enabled"
	BackupTargetLabel = "hornkeeper.noqer.com/backup-target"
	ReplicasLabel     = "hornkeeper.noqer.com/replicas"
	LonghornDriver    = "driver.longhorn.io"
)

var (
	VolumeGVK       = schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "Volume"}
	BackupTargetGVK = schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "BackupTarget"}
)

func Resource(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
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
