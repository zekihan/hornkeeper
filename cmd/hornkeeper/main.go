package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	controllerconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/zekihan/hornkeeper/internal/config"
	"github.com/zekihan/hornkeeper/internal/controller"
)

var version = "dev"

const (
	startupTimeout          = 30 * time.Second
	gracefulShutdownTimeout = 20 * time.Second
	processShutdownTimeout  = gracefulShutdownTimeout + 5*time.Second
	reconciliationTimeout   = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("Hornkeeper stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Default()
	var showVersion bool
	var logLevel string
	flag.StringVar(
		&cfg.LonghornNamespace,
		"longhorn-namespace",
		cfg.LonghornNamespace,
		"Namespace containing Longhorn volumes and backup targets",
	)
	flag.StringVar(
		&cfg.BackupTarget,
		"default-backup-target",
		cfg.BackupTarget,
		"Default backup target for enabled PVCs",
	)
	flag.IntVar(
		&cfg.Replicas,
		"default-replicas",
		cfg.Replicas,
		"Default replica count for enabled PVCs (1-20)",
	)
	flag.DurationVar(
		&cfg.ResyncInterval,
		"resync-interval",
		cfg.ResyncInterval,
		"Periodic retry interval for enabled PVCs",
	)
	flag.BoolVar(
		&cfg.LeaderElection,
		"leader-elect",
		cfg.LeaderElection,
		"Use a Lease to elect one active controller",
	)
	flag.StringVar(
		&cfg.LeaderNamespace,
		"leader-election-namespace",
		cfg.LeaderNamespace,
		"Namespace for the leader election Lease",
	)
	flag.StringVar(
		&cfg.MetricsAddress,
		"metrics-bind-address",
		cfg.MetricsAddress,
		"HTTP metrics address; 0 disables metrics",
	)
	flag.StringVar(
		&cfg.HealthAddress,
		"health-probe-bind-address",
		cfg.HealthAddress,
		"HTTP health probe address",
	)
	flag.BoolVar(
		&cfg.DryRun,
		"dry-run",
		cfg.DryRun,
		"Log intended volume changes without applying them",
	)
	flag.StringVar(&logLevel, "log-level", "info", "Log level: debug, info, warn, error")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf(
			"unexpected positional arguments: %q; configuration must use flags",
			flag.Args(),
		)
	}
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
	return runWithShutdownTimeout(ctx, processShutdownTimeout, func() error {
		return runController(ctx, &cfg)
	})
}

func runWithShutdownTimeout(ctx context.Context, timeout time.Duration, start func() error) error {
	done := make(chan error, 1)
	go func() { done <- start() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	// The manager's graceful shutdown timeout cannot help while startup is blocked.
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("controller shutdown exceeded %s: %w", timeout, context.DeadlineExceeded)
	}
}

func runController(ctx context.Context, cfg *config.Config) error {
	apiConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	apiConfig.UserAgent = "hornkeeper/" + version
	mgr, err := newManager(apiConfig, cfg)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := controller.CheckBackupTarget(
		startupCtx,
		mgr.GetAPIReader(),
		cfg.LonghornNamespace,
		cfg.BackupTarget,
	); err != nil {
		return fmt.Errorf("validate default backup target at startup: %w", err)
	}
	c := mgr.GetClient()
	if cfg.DryRun {
		// Any write that reaches the API server is validated but never persisted.
		c = client.NewDryRunClient(c)
	}
	r := &controller.Reconciler{
		Client:  c,
		Reader:  mgr.GetAPIReader(),
		Config:  *cfg,
		Metrics: controller.NewMetrics(metrics.Registry),
	}
	if err := r.SetupWithManager(ctx, mgr); err != nil {
		return fmt.Errorf("set up watches: %w", err)
	}
	ready := &cacheReady{cache: mgr.GetCache()}
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
	if cfg.MetricsAddress != "0" && !strings.HasPrefix(cfg.MetricsAddress, "127.0.0.1:") &&
		!strings.HasPrefix(cfg.MetricsAddress, "[::1]:") &&
		!strings.HasPrefix(cfg.MetricsAddress, "localhost:") {
		slog.WarnContext(
			ctx,
			"Metrics endpoint exposed on non-localhost address without authentication; consider restricting network access",
			slog.String("address", cfg.MetricsAddress),
		)
	}
	slog.InfoContext(
		ctx,
		"Starting hornkeeper",
		slog.String("version", version),
		slog.String("longhornNamespace", cfg.LonghornNamespace),
		slog.String("defaultBackupTarget", cfg.BackupTarget),
		slog.Int("defaultReplicas", cfg.Replicas),
		slog.Bool("leaderElection", cfg.LeaderElection),
		slog.Bool("dryRun", cfg.DryRun),
	)
	return mgr.Start(ctx)
}

func newManager(apiConfig *rest.Config, cfg *config.Config) (ctrl.Manager, error) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return ctrl.NewManager(apiConfig, ctrl.Options{
		Scheme:     scheme,
		Controller: controllerconfig.Controller{ReconciliationTimeout: reconciliationTimeout},
		MapperProvider: func(restConfig *rest.Config, httpClient *http.Client) (meta.RESTMapper, error) {
			// Discovery runs without the caller's context, including during manager creation.
			discoveryClient := *httpClient
			discoveryClient.Timeout = startupTimeout
			return apiutil.NewDynamicRESTMapper(restConfig, &discoveryClient)
		},
		Cache: cache.Options{
			ReaderFailOnMissingInformer: true,
			ByObject: map[client.Object]cache.ByObject{
				controller.Resource(controller.VolumeGVK): {
					Namespaces: map[string]cache.Config{cfg.LonghornNamespace: {}},
				},
				controller.Resource(controller.BackupTargetGVK): {
					Namespaces: map[string]cache.Config{cfg.LonghornNamespace: {}},
				},
			},
		},
		Metrics:                 metricsserver.Options{BindAddress: cfg.MetricsAddress},
		HealthProbeBindAddress:  cfg.HealthAddress,
		LeaderElection:          cfg.LeaderElection,
		LeaderElectionID:        "hornkeeper.noqer.com",
		LeaderElectionNamespace: cfg.LeaderNamespace,
		GracefulShutdownTimeout: new(gracefulShutdownTimeout),
	})
}

type cacheReady struct {
	cache  cache.Cache
	synced atomic.Bool
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

func (r *cacheReady) Check(_ *http.Request) error {
	if !r.synced.Load() {
		return errors.New("watch caches have not synchronized")
	}
	// Probes report local lifecycle state, not API availability. Never amplify
	// unauthenticated requests into API calls or make probes wait on the API.
	return nil
}
