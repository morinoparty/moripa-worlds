package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/morinoparty/moripa-worlds/internal/apiclient"
)

func TestStateCache(t *testing.T) {
	api := &fakeAPI{state: world(true, apiclient.StateStarting, "")}
	now := time.Unix(0, 0)
	var mu sync.Mutex
	c := newStateCache(api, func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	ctx := context.Background()
	for range 3 {
		if w, err := c.Get(ctx, wid, time.Second); err != nil || w.State != apiclient.StateStarting {
			t.Fatalf("Get = %+v, %v", w, err)
		}
	}
	if s, _ := api.counts(); s != 1 {
		t.Fatalf("states = %d", s)
	}
	mu.Lock()
	now = now.Add(2 * time.Second)
	mu.Unlock()
	api.set(func(f *fakeAPI) { f.state = world(true, apiclient.StateReady, "") })
	if w, _ := c.Get(ctx, wid, time.Second); w.State != apiclient.StateReady {
		t.Fatalf("stale: %+v", w)
	}
	c.Put(&apiclient.World{ID: wid, Found: true, State: apiclient.StateAbsent, Result: apiclient.ResultFull})
	if w, _ := c.Get(ctx, wid, time.Second); w.State != apiclient.StateAbsent || w.Result != "" {
		t.Fatalf("Put not used: %+v", w)
	}
	c.Invalidate(wid)
	if w, _ := c.Get(ctx, wid, time.Second); w.State != apiclient.StateReady {
		t.Fatalf("Invalidate not applied: %+v", w)
	}
}

func TestStateCacheConcurrentSingleflight(t *testing.T) {
	block := make(chan struct{})
	api := &fakeAPI{state: world(true, apiclient.StateStarting, ""), stateBlock: block}
	c := newStateCache(api, nil)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if _, err := c.Get(context.Background(), wid, time.Second); err != nil {
				t.Error(err)
			}
		})
	}
	eventually(t, "first call", func() bool { s, _ := api.counts(); return s >= 1 })
	time.Sleep(20 * time.Millisecond)
	close(block)
	wg.Wait()
	if s, _ := api.counts(); s != 1 {
		t.Fatalf("states = %d, want 1", s)
	}
}
