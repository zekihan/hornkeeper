package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cache"
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
