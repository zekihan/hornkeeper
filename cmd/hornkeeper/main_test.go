package main

import (
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

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
