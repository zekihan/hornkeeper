package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zekihan/hornkeeper/internal/config"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestStartupDiscoveryHasDeadline(t *testing.T) {
	stop := errors.New("stop after discovery request")
	apiConfig := &rest.Config{
		Host: "https://api.example.invalid",
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/api" {
				t.Errorf("request path = %q, want /api discovery", req.URL.Path)
			}
			if deadline, ok := req.Context().Deadline(); !ok || time.Until(deadline) > startupTimeout {
				t.Errorf("discovery must have a request deadline within %s, got %v (set: %v)", startupTimeout, deadline, ok)
			}
			return nil, stop
		}),
	}
	cfg := config.Default()
	cfg.LeaderElection = false
	cfg.HealthAddress = "0"
	if _, err := newManager(apiConfig, cfg); !errors.Is(err, stop) {
		t.Fatalf("create manager = %v, want discovery transport error", err)
	}
}

type blockedCacheSync struct {
	cache.Cache
	waiting chan struct{}
	release chan struct{}
}

func (c *blockedCacheSync) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (c *blockedCacheSync) WaitForCacheSync(ctx context.Context) bool {
	close(c.waiting)
	select {
	case <-c.release:
		return true
	case <-ctx.Done():
		return false
	}
}

func TestShutdownWhileCacheSyncIsBlocked(t *testing.T) {
	blocked := &blockedCacheSync{waiting: make(chan struct{}), release: make(chan struct{})}
	apiConfig := &rest.Config{
		Host: "https://api.example.invalid",
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("unexpected API request")
		}),
	}
	mgr, err := ctrl.NewManager(apiConfig, ctrl.Options{
		NewCache:                func(*rest.Config, cache.Options) (cache.Cache, error) { return blocked, nil },
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
		GracefulShutdownTimeout: new(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	managerDone := make(chan struct{})
	t.Cleanup(func() {
		close(blocked.release)
		select {
		case <-managerDone:
		case <-time.After(3 * time.Second):
			t.Error("manager did not stop after releasing cache sync")
		}
	})
	result := make(chan error, 1)
	go func() {
		result <- runWithShutdownTimeout(ctx, 20*time.Millisecond, func() error {
			defer close(managerDone)
			return mgr.Start(ctx)
		})
	}()
	select {
	case <-blocked.waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("manager did not reach cache sync")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not finish while cache sync was blocked")
	}
}

type readinessCache struct {
	cache.Cache
	syncResult   bool
	synchronized chan struct{}
}

func (c readinessCache) WaitForCacheSync(context.Context) bool {
	close(c.synchronized)
	return c.syncResult
}

// Readiness must work without an API reader, including for real HTTP requests.
// An unavailable API must not turn probes into API calls or block them.
func TestReadinessHTTPIsLocal(t *testing.T) {
	r := &cacheReady{}
	handler := http.StripPrefix("/readyz", &healthz.Handler{Checks: map[string]healthz.Checker{"cache": r.Check}})
	for _, synced := range []bool{false, true, false} {
		r.synced.Store(synced)
		for range 10 {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			want := http.StatusInternalServerError
			if synced {
				want = http.StatusOK
			}
			if response.Code != want {
				t.Fatalf("synced=%v: got status %d, want %d", synced, response.Code, want)
			}
		}
	}
}

func TestReadinessAndShutdown(t *testing.T) {
	for _, synced := range []bool{false, true} {
		c := readinessCache{syncResult: synced, synchronized: make(chan struct{})}
		r := &cacheReady{cache: c}
		if r.NeedLeaderElection() {
			t.Fatal("standby must also warm caches")
		}
		if err := r.Check(nil); err == nil {
			t.Fatal("ready before synchronization")
		}
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		wg.Go(func() {
			if err := r.Start(ctx); err != nil {
				t.Error(err)
			}
		})
		<-c.synchronized
		// For the successful path, wait for the readiness publication in Start.
		if synced {
			deadline := time.After(time.Second)
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for !r.synced.Load() {
				select {
				case <-deadline:
					t.Fatal("readiness was not published")
				case <-ticker.C:
				}
			}
			if err := r.Check(nil); err != nil {
				t.Fatal(err)
			}
		} else if err := r.Check(nil); err == nil {
			t.Fatal("ready after failed synchronization")
		}
		cancel()
		wg.Wait()
		if err := r.Check(nil); err == nil {
			t.Fatal("ready after shutdown")
		}
	}
}

// Keep every invocation safe even before argument validation is implemented:
// --version exits locally, and zero replicas fails before Kubernetes config loads.
func TestRunRejectsPositionalArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--version", "config.yaml"},
		{"--version", "--", "config.yaml"},
		{"--default-replicas=0", "false", "--leader-elect=false"},
		{"--default-replicas=0", "config.yaml"},
		{"--default-replicas=0", "config.yaml", "--default-replicas=2"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			originalFlags, originalArgs := flag.CommandLine, os.Args
			t.Cleanup(func() {
				flag.CommandLine, os.Args = originalFlags, originalArgs
			})
			flag.CommandLine = flag.NewFlagSet("hornkeeper", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = append([]string{"hornkeeper"}, args...)
			if err := run(); err == nil || !strings.Contains(err.Error(), "unexpected positional arguments") {
				t.Fatalf("run(%q) error = %v, want unexpected positional arguments", args, err)
			}
		})
	}
}

func TestRunValidatesFlagsBeforeLoadingKubernetesConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"invalid log level", []string{"--log-level=verbose"}, "invalid log level"},
		{"empty log level", []string{"--log-level="}, "invalid log level"},
		{"invalid namespace", []string{"--longhorn-namespace=Invalid"}, "invalid Longhorn namespace"},
		{"empty namespace", []string{"--longhorn-namespace="}, "invalid Longhorn namespace"},
		{"invalid target", []string{"--default-backup-target="}, "invalid default backup target"},
		{"malformed target", []string{"--default-backup-target=Bad_Name"}, "invalid default backup target"},
		{"invalid replicas", []string{"--default-replicas=0"}, "invalid default replicas"},
		{"negative replicas", []string{"--default-replicas=-1"}, "invalid default replicas"},
		{"too many replicas", []string{"--default-replicas=21"}, "invalid default replicas"},
		{"invalid interval", []string{"--resync-interval=0s"}, "resync interval must be positive"},
		{"negative interval", []string{"--resync-interval=-1s"}, "resync interval must be positive"},
		{"invalid leader namespace", []string{"--leader-election-namespace=Invalid"}, "invalid leader election namespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalFlags, originalArgs, originalLogger := flag.CommandLine, os.Args, slog.Default()
			t.Cleanup(func() {
				flag.CommandLine, os.Args = originalFlags, originalArgs
				slog.SetDefault(originalLogger)
			})
			flag.CommandLine = flag.NewFlagSet("hornkeeper", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = append([]string{"hornkeeper"}, tc.args...)
			if err := run(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run(%q) error = %v, want %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestRunWithShutdownTimeout(t *testing.T) {
	startErr := errors.New("startup failed")
	if err := runWithShutdownTimeout(t.Context(), time.Second, func() error { return startErr }); !errors.Is(err, startErr) {
		t.Fatalf("startup error = %v, want %v", err, startErr)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runWithShutdownTimeout(ctx, time.Second, func() error { return startErr }); !errors.Is(err, startErr) {
		t.Fatalf("shutdown error = %v, want %v", err, startErr)
	}

	release := make(chan struct{})
	defer close(release)
	if err := runWithShutdownTimeout(ctx, time.Millisecond, func() error { <-release; return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked shutdown error = %v, want deadline exceeded", err)
	}
}

func TestRunVersionSkipsValidation(t *testing.T) {
	originalFlags, originalArgs, originalStdout := flag.CommandLine, os.Args, os.Stdout
	t.Cleanup(func() {
		flag.CommandLine, os.Args, os.Stdout = originalFlags, originalArgs, originalStdout
	})
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	flag.CommandLine = flag.NewFlagSet("hornkeeper", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	// Invalid replicas would fail validation if --version did not return first.
	os.Args = []string{"hornkeeper", "--default-replicas=0", "--version"}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	os.Stdout = originalStdout
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != version+"\n" {
		t.Fatalf("version output = %q, want %q", out, version+"\n")
	}
}

func TestRunStopsWhenKubernetesConfigIsMissing(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("KUBERNETES_MASTER", "")
	if _, err := ctrl.GetConfig(); err == nil || !strings.Contains(err.Error(), "no configuration has been provided") {
		t.Fatalf("preflight config error = %v, want an empty kubeconfig", err)
	}
	originalFlags, originalArgs, originalLogger := flag.CommandLine, os.Args, slog.Default()
	t.Cleanup(func() {
		flag.CommandLine, os.Args = originalFlags, originalArgs
		slog.SetDefault(originalLogger)
	})
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	flag.CommandLine = flag.NewFlagSet("hornkeeper", flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	// Election is off, so an invalid lease namespace must not fail closed.
	os.Args = []string{"hornkeeper", "--leader-elect=false", "--leader-election-namespace=Invalid", "--log-level=debug"}
	err := run()
	if err == nil || !strings.Contains(err.Error(), "load Kubernetes configuration") {
		t.Fatalf("run error = %v, want missing Kubernetes configuration", err)
	}
	if strings.Contains(err.Error(), "leader election") {
		t.Fatalf("unused leader namespace was validated: %v", err)
	}
	if strings.Contains(logs.String(), "Starting hornkeeper") {
		t.Fatalf("controller started without configuration: %s", logs.String())
	}
}
