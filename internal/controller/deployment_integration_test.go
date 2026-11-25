//go:build integration

package controller

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	corev1 "k8s.io/api/core/v1"
	kerror "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/zekihan/hornkeeper/internal/config"
)

func TestExamplePermissionsAndLeaderFailover(t *testing.T) {
	_, apiConfig, scheme, admin := startExampleEnv(t)
	ctx := t.Context()
	seedExampleEnv(ctx, t, admin)
	volume, target := createExampleVolume(ctx, t, admin)
	saConfig := exampleSAConfig(apiConfig)
	sa, err := client.New(saConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(
		t,
		func() bool { return CheckBackupTarget(ctx, sa, "longhorn", "default") == nil },
		"example RBAC propagation",
	)
	assertForbiddenWrites(ctx, t, sa, volume, target)
	enableExampleClaim(ctx, t, admin, volume)
	first := startExampleManager(ctx, t, saConfig, scheme)
	firstStopped := false
	t.Cleanup(func() {
		if !firstStopped {
			stopExampleManager(t, first)
		}
	})
	waitFor(t, func() bool {
		return testutil.ToFloat64(
			first.metrics.Fields.WithLabelValues("numberOfReplicas", "patched"),
		) > 0
	}, "first leader patch under example RBAC")
	second := startExampleManager(ctx, t, saConfig, scheme)
	t.Cleanup(func() { stopExampleManager(t, second) })
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
	stopExampleManager(t, first)
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
	_ = unstructured.SetNestedField(volume.Object, int64(4), "spec", "numberOfReplicas")
	if err := admin.Update(ctx, volume); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		if err := admin.Get(ctx, client.ObjectKeyFromObject(volume), volume); err != nil {
			return false
		}
		count, _, _ := unstructured.NestedInt64(volume.Object, "spec", "numberOfReplicas")
		return count == 3 &&
			testutil.ToFloat64(
				second.metrics.Fields.WithLabelValues("numberOfReplicas", "patched"),
			) > 0
	}, "drift correction after leader failover")
}

// startExampleEnv starts the disposable envtest API server and admin client.
func startExampleEnv(
	t *testing.T,
) (*envtest.Environment, *rest.Config, *runtime.Scheme, client.Client) {
	t.Helper()
	// envtest launches its own etcd and API server; it never loads kubeconfig.
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{"testdata"},
		ErrorIfCRDPathMissing: true,
		UseExistingCluster:    new(false),
	}
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

	return environment, apiConfig, scheme, admin
}

// seedExampleEnv creates the example namespaces and manifest resources.
func seedExampleEnv(ctx context.Context, t *testing.T, admin client.Client) {
	t.Helper()
	for _, ns := range []string{"longhorn", "foreign"} {
		if err := admin.Create(
			ctx,
			&corev1.Namespace{Name: ns},
		); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"../../deploy/namespace.yaml", "../../deploy/rbac.yaml", "../../deploy/deployment.yaml", "../../deploy/service.yaml", "../../examples/pvc.yaml"} {
		applyExample(t, admin, file)
	}
}

// createExampleVolume creates the backup target and policy volume for the test.
func createExampleVolume(
	ctx context.Context,
	t *testing.T,
	admin client.Client,
) (*unstructured.Unstructured, *unstructured.Unstructured) {
	t.Helper()
	target := Resource(BackupTargetGVK)
	target.SetNamespace("longhorn")
	target.SetName("default")
	if err := admin.Create(ctx, target); err != nil {
		t.Fatal(err)
	}
	volume := Resource(VolumeGVK)
	volume.SetNamespace("longhorn")
	volume.SetName("policy-volume")
	volume.Object["spec"] = map[string]any{
		"backupTargetName": "default",
		"numberOfReplicas": int64(1),
		"size":             "1073741824",
		"dataLocality":     "disabled",
	}
	if err := admin.Create(ctx, volume); err != nil {
		t.Fatal(err)
	}

	return volume, target
}

// exampleSAConfig returns a config impersonating the example service account.
func exampleSAConfig(apiConfig *rest.Config) *rest.Config {
	saConfig := rest.CopyConfig(apiConfig)
	saConfig.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:hornkeeper:hornkeeper",
		Groups: []string{
			"system:serviceaccounts",
			"system:serviceaccounts:hornkeeper",
			"system:authenticated",
		},
	}

	return saConfig
}

// assertForbiddenWrites checks every RBAC denial the service account gets.
func assertForbiddenWrites(
	ctx context.Context,
	t *testing.T,
	sa client.Client,
	volume, target *unstructured.Unstructured,
) {
	t.Helper()
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
	_ = unstructured.SetNestedField(volume.Object, int64(2), "spec", "numberOfReplicas")
	if err := sa.Patch(
		ctx,
		volume,
		client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}),
	); err != nil {
		t.Fatal(err)
	}
	forbidden := func(err error) {
		t.Helper()
		if !kerror.IsForbidden(err) {
			t.Fatalf("expected forbidden, got %v", err)
		}
	}
	forbidden(sa.Update(ctx, volume))
	forbidden(
		sa.Status().
			Patch(ctx, volume, client.RawPatch("application/merge-patch+json", []byte(`{"status":{"ownerID":"forbidden"}}`))),
	)
	forbidden(sa.Update(ctx, target))
	forbidden(
		sa.Get(ctx, client.ObjectKey{Namespace: "hornkeeper", Name: "secret"}, &corev1.Secret{}),
	)
	foreign := Resource(VolumeGVK)
	forbidden(sa.Get(ctx, client.ObjectKey{Namespace: "foreign", Name: "volume"}, foreign))
}

// enableExampleClaim enables the example claim and binds it through a PV.
func enableExampleClaim(
	ctx context.Context,
	t *testing.T,
	admin client.Client,
	volume *unstructured.Unstructured,
) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := admin.Get(
		ctx,
		client.ObjectKey{Namespace: "default", Name: "application-data"},
		pvc,
	); err != nil {
		t.Fatal(err)
	}
	pvc.Labels, pvc.Annotations = map[string]string{EnabledLabel: "true"}, nil
	if err := admin.Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	pv := &corev1.PersistentVolume{
		Name: "policy-pv",
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      pvc.Spec.Resources.Requests,
			AccessModes:                   pvc.Spec.AccessModes,
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef: &corev1.ObjectReference{
				Namespace: pvc.Namespace,
				Name:      pvc.Name,
				UID:       pvc.UID,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       LonghornDriver,
					VolumeHandle: volume.GetName(),
				},
			},
		},
	}
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
}

type exampleManager struct {
	manager ctrl.Manager
	metrics *Metrics
	cancel  context.CancelFunc
	done    chan error
}

// startExampleManager starts one controller manager in the example env.
func startExampleManager(
	ctx context.Context,
	t *testing.T,
	saConfig *rest.Config,
	scheme *runtime.Scheme,
) exampleManager {
	t.Helper()
	mgr, err := ctrl.NewManager(saConfig, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
		Controller:              controllerconfig.Controller{SkipNameValidation: new(true)},
		LeaderElection:          true,
		LeaderElectionNamespace: "hornkeeper",
		LeaderElectionID:        "hornkeeper.noqer.com",
		LeaseDuration: new(
			3 * time.Second,
		),
		RenewDeadline: new(2 * time.Second),
		RetryPeriod:   new(500 * time.Millisecond),
		Cache: cache.Options{
			ReaderFailOnMissingInformer: true,
			ByObject: map[client.Object]cache.ByObject{
				Resource(VolumeGVK): {
					Namespaces: map[string]cache.Config{"longhorn": {}},
				},
				Resource(BackupTargetGVK): {
					Namespaces: map[string]cache.Config{"longhorn": {}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ResyncInterval = 5 * time.Minute
	m := NewMetrics(prometheus.NewRegistry())
	r := &Reconciler{
		Client:  mgr.GetClient(),
		Reader:  mgr.GetAPIReader(),
		Config:  cfg,
		Metrics: m,
	}
	if err := r.SetupWithManager(ctx, mgr); err != nil {
		t.Fatal(err)
	}
	managerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(managerCtx) }()

	return exampleManager{manager: mgr, metrics: m, cancel: cancel, done: done}
}

// stopExampleManager stops a started example manager.
func stopExampleManager(t *testing.T, i exampleManager) {
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
		if err := decoder.Decode(&obj.Object); errors.Is(err, io.EOF) {
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
