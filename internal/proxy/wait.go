package proxy

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/morinoparty/moripa-worlds/internal/apiclient"
)

// Player は待機ループが使うプレイヤーの操作 (Gate の Player を包む)。
type Player interface {
	// Key はプレイヤーを区別する値 (UUID)。
	Key() string
	Name() string
	// Context はプレイヤーがプロキシから切断すると終わる。
	Context() context.Context
	// CurrentServer は接続し終えた (PLAY に入った) サーバーの名前。なければ ""。
	CurrentServer() string
	ActionBar(msg string)
	Disconnect(msg string)
	// Connect は登録済みのサーバー name へ移す。移れたら true。
	Connect(ctx context.Context, name string) bool
}

// Servers は Gate へのサーバー登録。
type Servers interface {
	// Ensure は name を addr で登録する (既に同じものがあれば何もしない)。
	Ensure(name, addr string) error
}

// Waiter はロビーでワールドの起動を待つプレイヤーごとのループを管理する。
//
// 状態の問い合わせは stateCache を通すので、同じワールドを待つ人数によらず
// API への問い合わせはワールドごとに PollInterval に 1 回程度になる。
// ループはプレイヤーの切断・ワールドへの移動・Close のいずれかで必ず終わる。
type Waiter struct {
	API     API
	Servers Servers
	Lobby   string
	Log     *slog.Logger
	Metrics *Metrics

	// StartupWait は待つ上限 (コントローラーの STARTUP_TIMEOUT に揃える)。
	StartupWait time.Duration
	// PollInterval は状態を見る間隔 (既定 1 秒)。
	PollInterval time.Duration
	// ActionBarInterval はアクションバーを送り直す間隔 (既定 2 秒)。
	ActionBarInterval time.Duration
	// ConnectTimeout はワールドへの 1 回の接続の上限 (既定 15 秒)。
	// Gate 自身の connectionTimeout は v0.74.32 では使い物にならない (README 参照) ので必ず掛ける。
	ConnectTimeout time.Duration
	// MaxConnectAttempts はワールドへの接続を試す回数 (既定 3)。
	MaxConnectAttempts int
	// EnsureInterval は absent のときに ensure し直す最短の間隔 (既定 5 秒)。
	EnsureInterval time.Duration
	// Context が終わるとすべての待機ループを止める (プロキシの停止)。nil なら Close まで動く。
	Context context.Context

	cache   *stateCache
	once    sync.Once
	root    context.Context
	stopAll context.CancelFunc

	mu     sync.Mutex
	active map[string]*waitEntry
	wg     sync.WaitGroup
}

type waitEntry struct {
	cancel context.CancelFunc
}

// WaitOptions は Start の追加の指定。
type WaitOptions struct {
	// World は分かっていれば最初の状態 (タイトル表示用)。
	World *apiclient.World
	// Ensured は直前に ensure 済みなら true (すぐには ensure し直さない)。
	Ensured bool
	// ConnectDelay は最初の接続を試すまで待つ時間 (ワールドから落とされた直後など)。
	ConnectDelay time.Duration
}

func (w *Waiter) init() {
	w.once.Do(func() {
		if w.cache == nil {
			w.cache = newStateCache(w.API, nil)
		}
		parent := w.Context
		if parent == nil {
			parent = context.Background()
		}
		w.root, w.stopAll = context.WithCancel(parent)
		w.active = map[string]*waitEntry{}
		if w.Log == nil {
			w.Log = slog.Default()
		}
	})
}

func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Waiting は待っているプレイヤーの数。
func (w *Waiter) Waiting() int {
	w.init()
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.active)
}

// Start は p についてワールド id の起動を待つループを始める。既にあれば置き換える。
func (w *Waiter) Start(p Player, id string, opts WaitOptions) {
	w.init()
	ctx, cancel := context.WithCancel(p.Context())
	stop := context.AfterFunc(w.root, cancel)
	e := &waitEntry{cancel: cancel}

	w.mu.Lock()
	if old, ok := w.active[p.Key()]; ok {
		old.cancel()
	}
	w.active[p.Key()] = e
	w.mu.Unlock()

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer stop()
		defer cancel()
		defer func() {
			w.mu.Lock()
			if w.active[p.Key()] == e {
				delete(w.active, p.Key())
			}
			w.mu.Unlock()
		}()
		w.loop(ctx, p, id, opts)
	}()
}

// Stop は p の待機ループを止める。
func (w *Waiter) Stop(key string) {
	w.init()
	w.mu.Lock()
	if e, ok := w.active[key]; ok {
		e.cancel()
	}
	w.mu.Unlock()
}

// Close はすべての待機ループを止めて、終わるまで待つ。
func (w *Waiter) Close() {
	w.init()
	if w.stopAll != nil {
		w.stopAll()
	}
	w.wg.Wait()
}

// jitter は d に 0〜20% の揺らぎを足す (同じ瞬間に始まった待機が揃って問い合わせないように)。
func jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int64N(int64(d)/5+1)) //nolint:gosec // 間隔の揺らぎで、暗号用途ではない
}

func (w *Waiter) loop(ctx context.Context, p Player, id string, opts WaitOptions) {
	poll := durationOr(w.PollInterval, time.Second)
	barEvery := durationOr(w.ActionBarInterval, 2*time.Second)
	connectTimeout := durationOr(w.ConnectTimeout, 15*time.Second)
	ensureEvery := durationOr(w.EnsureInterval, 5*time.Second)
	maxAttempts := w.MaxConnectAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	log := w.Log.With("player", p.Name(), "id", id)

	start := time.Now()
	deadline := start.Add(durationOr(w.StartupWait, 5*time.Minute))
	connectAfter := start.Add(opts.ConnectDelay)
	var lastEnsure, lastBar time.Time
	if opts.Ensured {
		lastEnsure = start
	}
	title := "morino.party world"
	if opts.World != nil {
		title = opts.World.DisplayTitle()
	}
	attempts, errStreak := 0, 0
	wait := time.Duration(0)
	log.Info("waiting for world in lobby")

	for {
		if wait > 0 {
			t := time.NewTimer(jitter(wait))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		} else if ctx.Err() != nil {
			return
		}
		wait = poll

		now := time.Now()
		if now.After(deadline) {
			log.Info("world did not start in time")
			w.Metrics.connect("timeout")
			p.Disconnect(MsgStartTimeout)
			return
		}
		bar := func() {
			if now.Sub(lastBar) >= barEvery {
				p.ActionBar(MsgStarting(sanitizeTitle(title), int(now.Sub(start).Seconds())))
				lastBar = now
			}
		}

		st, err := w.cache.Get(ctx, id, poll*9/10)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			bar()
			// API が落ちている間は間隔を空けて待ち続ける (期限までに戻れば続行)。
			errStreak++
			wait = min(poll<<min(errStreak, 3), 5*time.Second)
			log.Warn("failed to get world state", "error", err, "streak", errStreak)
			continue
		}
		errStreak = 0
		if !st.Found {
			w.Metrics.connect("not_found")
			p.Disconnect(MsgNotFoundID(id))
			return
		}
		title = st.DisplayTitle()
		bar()

		switch st.State {
		case apiclient.StateAbsent:
			// 削除と入れ違った・クラッシュした等。間隔を空けて起動し直してもらう。
			if now.Sub(lastEnsure) < ensureEvery {
				continue
			}
			lastEnsure = now
			e, err := w.API.Ensure(ctx, id)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warn("failed to ensure world", "error", err)
				continue
			}
			w.cache.Put(e)
			switch d := decideEnsure(e); d.Action {
			case ActDisconnect:
				log.Info("giving up waiting", "result", e.Result)
				w.Metrics.connect(resultLabel(e.Result))
				p.Disconnect(d.Message)
				return
			default:
				continue
			}
		case apiclient.StateStarting:
			continue
		}

		// ready: ロビーに入り終えてから移す (ロビーの CONFIG 中に移そうとすると
		// Gate は「接続中」として断る)。
		if p.CurrentServer() != w.Lobby || now.Before(connectAfter) {
			continue
		}
		name := WorldServerName(id)
		if err := w.Servers.Ensure(name, st.Address); err != nil {
			log.Error("failed to register world server", "addr", st.Address, "error", err)
			w.Metrics.connect("error")
			p.Disconnect(MsgError)
			return
		}
		cctx, cancel := context.WithTimeout(ctx, connectTimeout)
		ok := p.Connect(cctx, name)
		cerr := cctx.Err()
		cancel()
		if ok {
			log.Info("moved player to world", "elapsed", time.Since(start).Round(time.Millisecond).String())
			w.Metrics.connect("success")
			return
		}
		if ctx.Err() != nil {
			return
		}
		attempts++
		w.cache.Invalidate(id)
		log.Warn("failed to move player to world", "attempt", attempts, "timeout", errIsTimeout(cerr))
		if attempts >= maxAttempts {
			w.Metrics.connect("gave_up")
			p.Disconnect(MsgConnectFailed)
			return
		}
		w.Metrics.connect("failed")
		wait = 2 * poll
	}
}

// resultLabel は ensure の結果を world_connects_total の result にする。
func resultLabel(r apiclient.Result) string {
	switch r {
	case apiclient.ResultNotFound:
		return "not_found"
	case apiclient.ResultFull, apiclient.ResultRateLimited:
		return "busy"
	}
	return "error"
}
