//go:build integration

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/zekihan/hornkeeper/internal/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestDisposableAPIServer(t *testing.T) {
	// envtest launches its own etcd and API server; it never loads kubeconfig.
	environment := &envtest.Environment{CRDDirectoryPaths: []string{"testdata"}, ErrorIfCRDPathMissing: true, UseExistingCluster: new(false)}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	apiConfig, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(apiConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, name := range []string{"storage", "app"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"default", "rustfs"} {
		target := Resource(BackupTargetGVK)
		target.SetName(name)
		target.SetNamespace("storage")
		if err := c.Create(ctx, target); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckBackupTarget(ctx, c, "storage", "rustfs"); err != nil {
		t.Fatal(err)
	}
	if err := CheckBackupTarget(ctx, c, "storage", "absent"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected missing target, got %v", err)
	}
	volume := Resource(VolumeGVK)
	volume.SetName("restored-handle")
	volume.SetNamespace("storage")
	volume.Object["spec"] = map[string]any{"numberOfReplicas": int64(1), "backupTargetName": "default", "size": "1073741824", "dataLocality": "disabled", "fromBackup": "s3://existing-backup", "nodeID": "original-node"}
	if err := c.Create(ctx, volume); err != nil {
		t.Fatal(err)
	}
	untouched := volume.DeepCopy()
	untouched.SetName("unlabelled-volume")
	untouched.SetUID("")
	untouched.SetResourceVersion("")
	if err := c.Create(ctx, untouched); err != nil {
		t.Fatal(err)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "restored", Labels: map[string]string{EnabledLabel: "true"}},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}, StorageClassName: new("")},
	}
	if err := c.Create(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(apiConfig, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache: cache.Options{ReaderFailOnMissingInformer: true, ByObject: map[client.Object]cache.ByObject{
			Resource(VolumeGVK):       {Namespaces: map[string]cache.Config{"storage": {}}},
			Resource(BackupTargetGVK): {Namespaces: map[string]cache.Config{"storage": {}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.LonghornNamespace = "storage"
	cfg.BackupTarget = "rustfs"
	cfg.Replicas = 2
	// Five minutes means the waits below prove event handling, not polling.
	cfg.ResyncInterval = 5 * time.Minute
	m := NewMetrics(prometheus.NewRegistry())
	r := &Reconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Config: cfg, Metrics: m}
	if err := r.SetupWithManager(ctx, mgr); err != nil {
		t.Fatal(err)
	}
	managerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(managerCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("manager did not shut down")
		}
	})
	waitFor(t, func() bool { return testutil.ToFloat64(m.Reconciles.WithLabelValues("enabled")) > 0 }, "initial pending claim reconciliation")
	assertSettings := func(target string, replicas int64) bool {
		v := Resource(VolumeGVK)
		if err := c.Get(ctx, client.ObjectKeyFromObject(volume), v); err != nil {
			return false
		}
		targetValue, _, _ := unstructured.NestedString(v.Object, "spec", "backupTargetName")
		count, _, _ := unstructured.NestedInt64(v.Object, "spec", "numberOfReplicas")
		return targetValue == target && count == replicas
	}
	if !assertSettings("default", 1) {
		t.Fatal("pending claim changed volume")
	}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "restored-pv"}, Spec: corev1.PersistentVolumeSpec{
		Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		ClaimRef:                      &corev1.ObjectReference{Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID},
		PersistentVolumeSource:        corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: LonghornDriver, VolumeHandle: volume.GetName()}},
	}}
	if err := c.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	pv.Status.Phase = corev1.VolumeBound
	if err := c.Status().Update(ctx, pv); err != nil {
		t.Fatal(err)
	}
	pvc.Spec.VolumeName = pv.Name
	if err := c.Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return assertSettings("rustfs", 2) }, "binding event and restored handle")
	updateLabels := func(labels map[string]string) {
		t.Helper()
		if err := c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
			t.Fatal(err)
		}
		pvc.Labels = labels
		if err := c.Update(ctx, pvc); err != nil {
			t.Fatal(err)
		}
	}
	updateLabels(map[string]string{EnabledLabel: "true", BackupTargetLabel: "default", ReplicasLabel: "3"})
	waitFor(t, func() bool { return assertSettings("default", 3) }, "override label event")
	updateLabels(map[string]string{EnabledLabel: "true"})
	waitFor(t, func() bool { return assertSettings("rustfs", 2) }, "override removal event")
	if err := c.Get(ctx, client.ObjectKeyFromObject(volume), volume); err != nil {
		t.Fatal(err)
	}
	stale := volume.DeepCopy()
	unstructured.SetNestedField(volume.Object, "concurrent-node", "spec", "nodeID")
	unstructured.SetNestedField(volume.Object, int64(4), "spec", "numberOfReplicas")
	if err := c.Update(ctx, volume); err != nil {
		t.Fatal(err)
	}
	base := stale.DeepCopy()
	unstructured.SetNestedField(stale.Object, "default", "spec", "backupTargetName")
	if err := c.Patch(ctx, stale, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); !apierrors.IsConflict(err) {
		t.Fatalf("expected real resourceVersion conflict, got %v", err)
	}
	waitFor(t, func() bool { return assertSettings("rustfs", 2) }, "Longhorn drift event")
	updateLabels(map[string]string{EnabledLabel: "true", BackupTargetLabel: "later", ReplicasLabel: "3"})
	waitFor(t, func() bool {
		return assertSettings("rustfs", 3) && testutil.ToFloat64(m.Fields.WithLabelValues("backupTargetName", "invalid")) > 0
	}, "independent invalid target")
	target := Resource(BackupTargetGVK)
	target.SetNamespace("storage")
	target.SetName("later")
	if err := c.Create(ctx, target); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return assertSettings("later", 3) }, "backup target creation event")
	updateLabels(map[string]string{EnabledLabel: "false", BackupTargetLabel: "default", ReplicasLabel: "1"})
	if err := c.Get(ctx, client.ObjectKeyFromObject(volume), volume); err != nil {
		t.Fatal(err)
	}
	unstructured.SetNestedField(volume.Object, int64(4), "spec", "numberOfReplicas")
	if err := c.Update(ctx, volume); err != nil {
		t.Fatal(err)
	}
	// Force a queued reconcile as well as allowing the volume watch to observe disabling.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pvc)}); err != nil {
		t.Fatal(err)
	}
	if !assertSettings("later", 4) {
		t.Fatal("disabled claim enforced settings")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(volume), volume); err != nil {
		t.Fatal(err)
	}
	if node, _, _ := unstructured.NestedString(volume.Object, "spec", "nodeID"); node != "concurrent-node" {
		t.Fatal("unrelated field changed")
	}
	if source, _, _ := unstructured.NestedString(volume.Object, "spec", "fromBackup"); source != "s3://existing-backup" {
		t.Fatal("restored-volume source changed")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(untouched), untouched); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := unstructured.NestedInt64(untouched.Object, "spec", "numberOfReplicas"); n != 1 {
		t.Fatal("unlabelled volume changed")
	}
}

func waitFor(t *testing.T, check func() bool, description string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", description)
		case <-ticker.C:
		}
	}
}
