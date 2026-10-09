package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/morinoparty/moripa-worlds/internal/apiclient"
)

type fakePlayer struct {
	key    string
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	server      string
	bars        []string
	disconnect  string
	connects    int
	connectOK   func(n int) bool
	connectedTo string
}

func newPlayer(key string) *fakePlayer {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakePlayer{key: key, ctx: ctx, cancel: cancel, server: "lobby", connectOK: func(int) bool { return true }}
}

func (p *fakePlayer) Key() string              { return p.key }
func (p *fakePlayer) Name() string             { return "player-" + p.key }
func (p *fakePlayer) Context() context.Context { return p.ctx }
func (p *fakePlayer) CurrentServer() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.server
}
func (p *fakePlayer) ActionBar(msg string) {
	p.mu.Lock()
	p.bars = append(p.bars, msg)
	p.mu.Unlock()
}
func (p *fakePlayer) Disconnect(msg string) {
	p.mu.Lock()
	p.disconnect = msg
	p.mu.Unlock()
	p.cancel() // Gate ではプレイヤーの Context が終わる
}
func (p *fakePlayer) Connect(ctx context.Context, name string) bool {
	p.mu.Lock()
	p.connects++
	n := p.connects
	ok := p.connectOK(n)
	if ok {
		p.server = name
		p.connectedTo = name
	}
	p.mu.Unlock()
	if _, has := ctx.Deadline(); !has {
		panic("connect without deadline")
	}
	return ok
}
func (p *fakePlayer) snapshot() (disconnect, connectedTo string, connects, bars int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disconnect, p.connectedTo, p.connects, len(p.bars)
}

type fakeServers struct {
	mu  sync.Mutex
	reg map[string]string
}

func (s *fakeServers) Ensure(name, addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reg == nil {
		s.reg = map[string]string{}
	}
	s.reg[name] = addr
	return nil
}

func newWaiter(api API) (*Waiter, *fakeServers) {
	srv := &fakeServers{}
	return &Waiter{
		API:               api,
		Servers:           srv,
		Lobby:             "lobby",
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		StartupWait:       2 * time.Second,
		PollInterval:      10 * time.Millisecond,
		ActionBarInterval: 20 * time.Millisecond,
		ConnectTimeout:    time.Second,
		EnsureInterval:    30 * time.Millisecond,
	}, srv
}

// eventually は cond が真になるまで待つ。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// closeWithin は Close がすぐ返る (ゴルーチンが残っていない) ことを確かめる。
func closeWithin(t *testing.T, w *Waiter) {
	t.Helper()
	done := make(chan struct{})
	go func() { w.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Waiter.Close did not return: wait loop leaked")
	}
	if n := w.Waiting(); n != 0 {
		t.Fatalf("Waiting = %d after Close", n)
	}
}

func TestWaitMovesPlayerWhenReady(t *testing.T) {
	api := &fakeAPI{state: world(true, apiclient.StateStarting, "")}
	w, srv := newWaiter(api)
	p := newPlayer("a")
	w.Start(p, wid, WaitOptions{Ensured: true})
	eventually(t, "action bar", func() bool { _, _, _, bars := p.snapshot(); return bars >= 2 })
	if w.Waiting() != 1 {
		t.Fatalf("Waiting = %d", w.Waiting())
	}
	api.set(func(f *fakeAPI) { f.state = world(true, apiclient.StateReady, "") })
	eventually(t, "connect", func() bool { _, to, _, _ := p.snapshot(); return to != "" })
	_, to, n, _ := p.snapshot()
	if to != "w-"+wid || n != 1 {
		t.Fatalf("connected to %q after %d attempts", to, n)
	}
	if srv.reg["w-"+wid] != "w-"+wid+".worlds.svc.cluster.local:25565" {
		t.Fatalf("registered = %v", srv.reg)
	}
	eventually(t, "loop exit", func() bool { return w.Waiting() == 0 })
	p.mu.Lock()
	bar := p.bars[0]
	p.mu.Unlock()
	if bar != MsgStarting("テストワールド", 0) {
		t.Fatalf("action bar = %q", bar)
	}
	if _, e := api.counts(); e != 0 {
		t.Fatalf("ensured %d times while starting", e)
	}
	closeWithin(t, w)
}

func TestWaitDoesNotConnectBeforeLobbyJoin(t *testing.T) {
	api := &fakeAPI{state: world(true, apiclient.StateReady, "")}
	w, _ := newWaiter(api)
	p := newPlayer("a")
	p.server = "" // まだロビーの CONFIG 中
	w.Start(p, wid, WaitOptions{})
	time.Sleep(80 * time.Millisecond)
	if _, _, n, _ := p.snapshot(); n != 0 {
		t.Fatalf("connected %d times before joining the lobby", n)
	}
	p.mu.Lock()
	p.server = "lobby"
	p.mu.Unlock()
	eventually(t, "connect", func() bool { _, to, _, _ := p.snapshot(); return to != "" })
	closeWithin(t, w)
}

func TestWaitRetriesThenGivesUp(t *testing.T) {
	api := &fakeAPI{state: world(true, apiclient.StateReady, "")}
	w, _ := newWaiter(api)
	p := newPlayer("a")
	p.connectOK = func(int) bool { return false }
	w.Start(p, wid, WaitOptions{})
	eventually(t, "disconnect", func() bool { d, _, _, _ := p.snapshot(); return d != "" })
	d, _, n, _ := p.snapshot()
	if d != MsgConnectFailed || n != 3 {
		t.Fatalf("disconnect %q after %d attempts", d, n)
	}
	closeWithin(t, w)
}

func TestWaitRetrySucceeds(t *testing.T) {
	api := &fakeAPI{state: world(true, apiclient.StateReady, "")}
	w, _ := newWaiter(api)
	p := newPlayer("a")
	p.connectOK = func(n int) bool { return n == 2 }
	w.Start(p, wid, WaitOptions{})
	eventually(t, "connect", func() bool { _, to, _, _ := p.snapshot(); return to != "" })
	if d, _, n, _ := p.snapshot(); d != "" || n != 2 {
		t.Fatalf("disconnect %q, attempts %d", d, n)
	}
	closeWithin(t, w)
}

func TestWaitTimeout(t *testing.T) {
	api := &fakeAPI{state: world(true, apiclient.StateStarting, "")}
	w, _ := newWaiter(api)
	w.StartupWait = 100 * time.Millisecond
	p := newPlayer("a")
	w.Start(p, wid, WaitOptions{})
	eventually(t, "disconnect", func() bool { d, _, _, _ := p.snapshot(); return d != "" })
	if d, _, _, _ := p.snapshot(); d != MsgStartTimeout {
		t.Fatalf("disconnect = %q", d)
	}
	closeWithin(t, w)
}

func TestWaitWorldDeleted(t *testing.T) {
	api := &fakeAPI{state: world(false, apiclient.StateAbsent, "")}
	w, _ := newWaiter(api)
	p := newPlayer("a")
	w.Start(p, wid, WaitOptions{})
	eventually(t, "disconnect", func() bool { d, _, _, _ := p.snapshot(); return d != "" })
	if d, _, _, _ := p.snapshot(); d != MsgNotFoundID(wid) {
		t.Fatalf("disconnect = %q", d)
	}
	closeWithin(t, w)
}

func TestWaitReEnsuresAbsentWorld(t *testing.T) {
	// 起動中に消えた (absent) ワールドは ensure し直し、起動したら移す。
	api := &fakeAPI{
		state:  world(true, apiclient.StateAbsent, ""),
		ensure: world(true, apiclient.StateStarting, apiclient.ResultCreated),
		onEnsure: func(f *fakeAPI) {
			f.state = world(true, apiclient.StateReady, "")
		},
	}
	w, _ := newWaiter(api)
	p := newPlayer("a")
	w.Start(p, wid, WaitOptions{})
	eventually(t, "connect", func() bool { _, to, _, _ := p.snapshot(); return to != "" })
	if _, e := api.counts(); e != 1 {
		t.Fatalf("ensures = %d", e)
	}
	closeWithin(t, w)
}

func TestWaitReEnsureFull(t *testing.T) {
	api := &fakeAPI{
		state:  world(true, apiclient.StateAbsent, ""),
		ensure: world(true, apiclient.StateAbsent, apiclient.ResultFull),
	}
	w, _ := newWaiter(api)
	p := newPlayer("a")
	w.Start(p, wid, WaitOptions{})
	eventually(t, "disconnect", func() bool { d, _, _, _ := p.snapshot(); return d != "" })
	if d, _, _, _ := p.snapshot(); d != MsgBusy {
		t.Fatalf("disconnect = %q", d)
	}
	closeWithin(t, w)
}

func TestWaitAPIErrorKeepsWaiting(t *testing.T) {
	api := &fakeAPI{stateErr: fmt.Errorf("down")}
	w, _ := newWaiter(api)
	p := newPlayer("a")
	w.Start(p, wid, WaitOptions{})
	eventually(t, "a few polls", func() bool { s, _ := api.counts(); return s >= 2 })
	if d, _, _, _ := p.snapshot(); d != "" {
		t.Fatalf("disconnected on transient API error: %q", d)
	}
	api.set(func(f *fakeAPI) { f.stateErr = nil; f.state = world(true, apiclient.StateReady, "") })
	eventually(t, "connect", func() bool { _, to, _, _ := p.snapshot(); return to != "" })
	closeWithin(t, w)
}

// N 人が同じワールドを待っても、API への問い合わせは 1 本にまとまる。
func TestWaitSingleflight(t *testing.T) {
	api := &fakeAPI{state: world(true, apiclient.StateStarting, "")}
	w, _ := newWaiter(api)
	w.PollInterval = 20 * time.Millisecond
	const players = 30
	ps := make([]*fakePlayer, players)
	for i := range ps {
		ps[i] = newPlayer(fmt.Sprint(i))
		w.Start(ps[i], wid, WaitOptions{Ensured: true})
	}
	eventually(t, "all waiting", func() bool { return w.Waiting() == players })
	time.Sleep(300 * time.Millisecond)
	states, _ := api.counts()
	// 300ms / 20ms ≒ 15 回。1 人 1 本なら 450 回前後になる。
	if states > 40 {
		t.Fatalf("State called %d times for %d waiting players", states, players)
	}
	api.set(func(f *fakeAPI) { f.state = world(true, apiclient.StateReady, "") })
	for _, p := range ps {
		eventually(t, "connect "+p.key, func() bool { _, to, _, _ := p.snapshot(); return to != "" })
	}
	closeWithin(t, w)
}

// プレイヤーが切断したら (Context が終わったら) ループは終わり、ゴルーチンは残らない。
func TestWaitCancellationNoLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	api := &fakeAPI{state: world(true, apiclient.StateStarting, "")}
	w, _ := newWaiter(api)
	ps := make([]*fakePlayer, 20)
	for i := range ps {
		ps[i] = newPlayer(fmt.Sprint(i))
		w.Start(ps[i], wid, WaitOptions{})
	}
	eventually(t, "all waiting", func() bool { return w.Waiting() == len(ps) })
	for _, p := range ps {
		p.cancel()
	}
	eventually(t, "all stopped", func() bool { return w.Waiting() == 0 })
	closeWithin(t, w)
	eventually(t, "goroutines released", func() bool { return runtime.NumGoroutine() <= before+2 })
}

// API の応答待ちの最中に切断しても止まる。
func TestWaitCancelWhileAPIBlocked(t *testing.T) {
	api := &fakeAPI{state: world(true, apiclient.StateStarting, ""), stateBlock: make(chan struct{})}
	w, _ := newWaiter(api)
	p := newPlayer("a")
	w.Start(p, wid, WaitOptions{})
	eventually(t, "blocked poll", func() bool { s, _ := api.counts(); return s >= 1 })
	p.cancel()
	eventually(t, "stopped", func() bool { return w.Waiting() == 0 })
	close(api.stateBlock)
	closeWithin(t, w)
}

// Start し直すと前のループは止まる。Stop / Close でも止まる。
func TestWaitReplaceStopClose(t *testing.T) {
	api := &fakeAPI{state: world(true, apiclient.StateStarting, "")}
	w, _ := newWaiter(api)
	p := newPlayer("a")
	w.Start(p, wid, WaitOptions{})
	w.Start(p, wid, WaitOptions{})
	eventually(t, "one loop", func() bool { return w.Waiting() == 1 })
	w.Stop(p.Key())
	eventually(t, "stopped", func() bool { return w.Waiting() == 0 })
	q := newPlayer("b")
	w.Start(q, wid, WaitOptions{})
	eventually(t, "waiting", func() bool { return w.Waiting() == 1 })
	closeWithin(t, w)
	if q.ctx.Err() != nil {
		t.Fatal("Close must not disconnect players")
	}
}

// ConnectDelay の間は ready でも移さない (落ちた直後のワールドへすぐ繋がない)。
func TestWaitConnectDelay(t *testing.T) {
	api := &fakeAPI{state: world(true, apiclient.StateReady, "")}
	w, _ := newWaiter(api)
	p := newPlayer("a")
	start := time.Now()
	w.Start(p, wid, WaitOptions{ConnectDelay: 150 * time.Millisecond})
	eventually(t, "connect", func() bool { _, to, _, _ := p.snapshot(); return to != "" })
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("connected before ConnectDelay")
	}
	closeWithin(t, w)
}
