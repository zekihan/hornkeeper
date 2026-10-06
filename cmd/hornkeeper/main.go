package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/zekihan/hornkeeper/internal/config"
	"github.com/zekihan/hornkeeper/internal/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("Hornkeeper stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Default()
	var showVersion bool
	var logLevel string
	flag.StringVar(&cfg.LonghornNamespace, "longhorn-namespace", cfg.LonghornNamespace, "Namespace containing Longhorn volumes and backup targets")
	flag.StringVar(&cfg.BackupTarget, "default-backup-target", cfg.BackupTarget, "Default backup target for enabled PVCs")
	flag.IntVar(&cfg.Replicas, "default-replicas", cfg.Replicas, "Default replica count for enabled PVCs (1-20)")
	flag.DurationVar(&cfg.ResyncInterval, "resync-interval", cfg.ResyncInterval, "Periodic retry interval for enabled PVCs")
	flag.BoolVar(&cfg.LeaderElection, "leader-elect", cfg.LeaderElection, "Use a Lease to elect one active controller")
	flag.StringVar(&cfg.LeaderNamespace, "leader-election-namespace", cfg.LeaderNamespace, "Namespace for the leader election Lease")
	flag.StringVar(&cfg.MetricsAddress, "metrics-bind-address", cfg.MetricsAddress, "HTTP metrics address; 0 disables metrics")
	flag.StringVar(&cfg.HealthAddress, "health-probe-bind-address", cfg.HealthAddress, "HTTP health probe address")
	flag.StringVar(&logLevel, "log-level", "info", "Log level: debug, info, warn, error")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.Parse()
	if showVersion {
		fmt.Println(version)
		return nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return fmt.Errorf("invalid log level: %w", err)
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
	ctrl.SetLogger(logr.FromSlogHandler(handler))
	if err := cfg.Validate(); err != nil {
		return err
	}
	ctx := ctrl.SetupSignalHandler()
	apiConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	apiConfig.UserAgent = "hornkeeper/" + version
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(apiConfig, ctrl.Options{
		Scheme:     scheme,
		Controller: controllerconfig.Controller{ReconciliationTimeout: 30 * time.Second},
		Cache: cache.Options{
			ReaderFailOnMissingInformer: true,
			ByObject: map[client.Object]cache.ByObject{
				controller.Resource(controller.VolumeGVK):       {Namespaces: map[string]cache.Config{cfg.LonghornNamespace: {}}},
				controller.Resource(controller.BackupTargetGVK): {Namespaces: map[string]cache.Config{cfg.LonghornNamespace: {}}},
			},
		},
		Metrics:                 metricsserver.Options{BindAddress: cfg.MetricsAddress},
		HealthProbeBindAddress:  cfg.HealthAddress,
		LeaderElection:          cfg.LeaderElection,
		LeaderElectionID:        "hornkeeper.noqer.com",
		LeaderElectionNamespace: cfg.LeaderNamespace,
		GracefulShutdownTimeout: new(20 * time.Second),
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	startupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := controller.CheckBackupTarget(startupCtx, mgr.GetAPIReader(), cfg.LonghornNamespace, cfg.BackupTarget); err != nil {
		return fmt.Errorf("validate default backup target at startup: %w", err)
	}
	// Verify Longhorn namespace exists.
	ns := &corev1.Namespace{}
	if err := mgr.GetAPIReader().Get(startupCtx, client.ObjectKey{Name: cfg.LonghornNamespace}, ns); err != nil {
		return fmt.Errorf("validate Longhorn namespace %q at startup: %w", cfg.LonghornNamespace, err)
	}
	r := &controller.Reconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Config: cfg, Metrics: controller.NewMetrics(metrics.Registry)}
	if err := r.SetupWithManager(ctx, mgr); err != nil {
		return fmt.Errorf("set up watches: %w", err)
	}
	ready := &cacheReady{cache: mgr.GetCache(), longhornNS: cfg.LonghornNamespace, reader: mgr.GetAPIReader()}
	if err := mgr.Add(ready); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("health", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("cache", ready.Check); err != nil {
		return err
	}
	// Warn if metrics are exposed on non-localhost without authentication.
	if cfg.MetricsAddress != "0" && cfg.MetricsAddress != ":8080" && !strings.HasPrefix(cfg.MetricsAddress, "127.0.0.1:") && !strings.HasPrefix(cfg.MetricsAddress, "[::1]:") && !strings.HasPrefix(cfg.MetricsAddress, "localhost:") {
		slog.WarnContext(ctx, "Metrics endpoint exposed on non-localhost address without authentication; consider restricting network access", "address", cfg.MetricsAddress)
	}
	slog.InfoContext(ctx, "Starting hornkeeper", "version", version, "longhornNamespace", cfg.LonghornNamespace,
		"defaultBackupTarget", cfg.BackupTarget, "defaultReplicas", cfg.Replicas, "leaderElection", cfg.LeaderElection)
	return mgr.Start(ctx)
}

type cacheReady struct {
	cache      cache.Cache
	longhornNS string
	reader     client.Reader
	synced     atomic.Bool
}

func (r *cacheReady) NeedLeaderElection() bool { return false }
func (r *cacheReady) Start(ctx context.Context) error {
	if r.cache.WaitForCacheSync(ctx) {
		r.synced.Store(true)
	}
	<-ctx.Done()
	r.synced.Store(false)
	return nil
}
func (r *cacheReady) Check(req *http.Request) error {
	if !r.synced.Load() {
		return fmt.Errorf("watch caches have not synchronized")
	}
	if req == nil {
		return nil // Allow nil for testing
	}
	// Verify Longhorn API is reachable by listing volumes.
	checkCtx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	defer cancel()
	volList := &unstructured.UnstructuredList{}
	volList.SetGroupVersionKind(controller.VolumeGVK)
	if err := r.reader.List(checkCtx, volList, client.InNamespace(r.longhornNS), client.Limit(1)); err != nil {
		return fmt.Errorf("Longhorn API not reachable: %w", err)
	}
	return nil
}
