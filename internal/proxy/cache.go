package proxy

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/morinoparty/moripa-worlds/internal/apiclient"
)

// stateCache はワールドの状態 (GET /v1/worlds/{id}) を短い間覚え、同じワールドへの同時の
// 問い合わせを 1 回にまとめる。ロビーで N 人が同じワールドを待っていても、API へは
// 1 秒に 1 回程度しか問い合わせない。
type stateCache struct {
	api API
	now func() time.Time

	mu sync.Mutex
	m  map[string]cacheEntry
	sf singleflight.Group
}

type cacheEntry struct {
	w  *apiclient.World
	at time.Time
}

func newStateCache(api API, now func() time.Time) *stateCache {
	if now == nil {
		now = time.Now
	}
	return &stateCache{api: api, now: now, m: map[string]cacheEntry{}}
}

// Get は maxAge より新しい結果があればそれを、なければ API に問い合わせて返す。
// エラーは覚えない。
func (c *stateCache) Get(ctx context.Context, id string, maxAge time.Duration) (*apiclient.World, error) {
	c.mu.Lock()
	e, ok := c.m[id]
	c.mu.Unlock()
	if ok && c.now().Sub(e.at) < maxAge {
		return e.w, nil
	}
	ch := c.sf.DoChan(id, func() (any, error) {
		// 最初の呼び出し元が先にいなくなっても、他の待機者に結果を返せるよう切り離す
		// (apiclient がタイムアウトを掛ける)。
		w, err := c.api.State(context.WithoutCancel(ctx), id)
		if err == nil {
			c.Put(w)
		}
		return w, err
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(*apiclient.World), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Put は w を最新の状態として覚える (ensure の応答など)。
func (c *stateCache) Put(w *apiclient.World) {
	if w == nil {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 1024 {
		for k, v := range c.m {
			if now.Sub(v.at) > time.Minute {
				delete(c.m, k)
			}
		}
	}
	cp := *w
	cp.Result = ""
	c.m[w.ID] = cacheEntry{w: &cp, at: now}
}

// Invalidate は id の結果を捨てる。
func (c *stateCache) Invalidate(id string) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}
