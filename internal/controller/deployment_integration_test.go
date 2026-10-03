//go:build integration

package controller

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/zekihan/hornkeeper/internal/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestExamplePermissionsAndLeaderFailover(t *testing.T) {
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
	admin, err := client.New(apiConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for _, ns := range []string{"longhorn", "foreign"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"../../deploy/namespace.yaml", "../../deploy/rbac.yaml", "../../deploy/deployment.yaml", "../../deploy/service.yaml", "../../examples/pvc.yaml"} {
		applyExample(t, admin, file)
	}
	target := Resource(BackupTargetGVK)
	target.SetNamespace("longhorn")
	target.SetName("default")
	if err := admin.Create(ctx, target); err != nil {
		t.Fatal(err)
	}
	volume := Resource(VolumeGVK)
	volume.SetNamespace("longhorn")
	volume.SetName("policy-volume")
	volume.Object["spec"] = map[string]any{"backupTargetName": "default", "numberOfReplicas": int64(1), "size": "1073741824", "dataLocality": "disabled"}
	if err := admin.Create(ctx, volume); err != nil {
		t.Fatal(err)
	}
	saConfig := rest.CopyConfig(apiConfig)
	saConfig.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:hornkeeper:hornkeeper",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:hornkeeper", "system:authenticated"},
	}
	sa, err := client.New(saConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return CheckBackupTarget(ctx, sa, "longhorn", "default") == nil }, "example RBAC propagation")
	if err := sa.List(ctx, &corev1.PersistentVolumeClaimList{}); err != nil {
		t.Fatal(err)
	}
	if err := sa.List(ctx, &corev1.PersistentVolumeList{}); err != nil {
		t.Fatal(err)
	}
	if err := sa.Get(ctx, client.ObjectKeyFromObject(volume), volume); err != nil {
		t.Fatal(err)
	}
	base := volume.DeepCopy()
	unstructured.SetNestedField(volume.Object, int64(2), "spec", "numberOfReplicas")
	if err := sa.Patch(ctx, volume, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatal(err)
	}
	forbidden := func(err error) {
		t.Helper()
		if !apierrors.IsForbidden(err) {
			t.Fatalf("expected forbidden, got %v", err)
		}
	}
	forbidden(sa.Update(ctx, volume))
	forbidden(sa.Status().Patch(ctx, volume, client.RawPatch("application/merge-patch+json", []byte(`{"status":{"ownerID":"forbidden"}}`))))
	forbidden(sa.Update(ctx, target))
	forbidden(sa.Get(ctx, client.ObjectKey{Namespace: "hornkeeper", Name: "secret"}, &corev1.Secret{}))
	foreign := Resource(VolumeGVK)
	forbidden(sa.Get(ctx, client.ObjectKey{Namespace: "foreign", Name: "volume"}, foreign))
	pvc := &corev1.PersistentVolumeClaim{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "application-data"}, pvc); err != nil {
		t.Fatal(err)
	}
	forbidden(sa.Update(ctx, pvc))
	pvc.Labels = map[string]string{EnabledLabel: "true"}
	if err := admin.Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "policy-pv"}, Spec: corev1.PersistentVolumeSpec{
		Capacity: pvc.Spec.Resources.Requests, AccessModes: pvc.Spec.AccessModes, PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		ClaimRef:               &corev1.ObjectReference{Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID},
		PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: LonghornDriver, VolumeHandle: volume.GetName()}},
	}}
	if err := admin.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	pv.Status.Phase = corev1.VolumeBound
	if err := admin.Status().Update(ctx, pv); err != nil {
		t.Fatal(err)
	}
	pvc.Spec.VolumeName = pv.Name
	if err := admin.Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := admin.Status().Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	type instance struct {
		manager ctrl.Manager
		metrics *Metrics
		cancel  context.CancelFunc
		done    chan error
	}
	start := func() instance {
		t.Helper()
		mgr, err := ctrl.NewManager(saConfig, ctrl.Options{
			Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
			Controller:     controllerconfig.Controller{SkipNameValidation: new(true)},
			LeaderElection: true, LeaderElectionNamespace: "hornkeeper", LeaderElectionID: "hornkeeper.noqer.com",
			LeaseDuration: new(3 * time.Second), RenewDeadline: new(2 * time.Second), RetryPeriod: new(500 * time.Millisecond),
			Cache: cache.Options{ReaderFailOnMissingInformer: true, ByObject: map[client.Object]cache.ByObject{
				Resource(VolumeGVK):       {Namespaces: map[string]cache.Config{"longhorn": {}}},
				Resource(BackupTargetGVK): {Namespaces: map[string]cache.Config{"longhorn": {}}},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.ResyncInterval = 5 * time.Minute
		m := NewMetrics(prometheus.NewRegistry())
		r := &Reconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Config: cfg, Metrics: m}
		if err := r.SetupWithManager(ctx, mgr); err != nil {
			t.Fatal(err)
		}
		managerCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- mgr.Start(managerCtx) }()
		return instance{manager: mgr, metrics: m, cancel: cancel, done: done}
	}
	stop := func(i instance) {
		t.Helper()
		i.cancel()
		select {
		case err := <-i.done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("manager shutdown timed out")
		}
	}
	first := start()
	firstStopped := false
	t.Cleanup(func() {
		if !firstStopped {
			stop(first)
		}
	})
	waitFor(t, func() bool {
		return testutil.ToFloat64(first.metrics.Fields.WithLabelValues("numberOfReplicas", "patched")) > 0
	}, "first leader patch under example RBAC")
	second := start()
	t.Cleanup(func() { stop(second) })
	cacheCtx, cacheCancel := context.WithTimeout(ctx, 10*time.Second)
	defer cacheCancel()
	if !second.manager.GetCache().WaitForCacheSync(cacheCtx) {
		t.Fatal("standby caches failed to synchronize")
	}
	select {
	case <-second.manager.Elected():
		t.Fatal("standby elected while first leader is active")
	default:
	}
	if testutil.ToFloat64(second.metrics.Reconciles.WithLabelValues("enabled")) != 0 {
		t.Fatal("standby reconciled before election")
	}
	stop(first)
	firstStopped = true
	waitFor(t, func() bool {
		select {
		case <-second.manager.Elected():
			return true
		default:
			return false
		}
	}, "standby leader takeover")
	if err := admin.Get(ctx, client.ObjectKeyFromObject(volume), volume); err != nil {
		t.Fatal(err)
	}
	unstructured.SetNestedField(volume.Object, int64(4), "spec", "numberOfReplicas")
	if err := admin.Update(ctx, volume); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		if err := admin.Get(ctx, client.ObjectKeyFromObject(volume), volume); err != nil {
			return false
		}
		count, _, _ := unstructured.NestedInt64(volume.Object, "spec", "numberOfReplicas")
		return count == 3 && testutil.ToFloat64(second.metrics.Fields.WithLabelValues("numberOfReplicas", "patched")) > 0
	}, "drift correction after leader failover")
}

func applyExample(t *testing.T, c client.Client, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); err == io.EOF {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := c.Create(t.Context(), obj); err != nil {
			t.Fatalf("apply example %s: %v", path, err)
		}
	}
}
