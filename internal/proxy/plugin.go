package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robinbraemer/event"
	"go.minekube.com/common/minecraft/color"
	"go.minekube.com/common/minecraft/component"
	"go.minekube.com/common/minecraft/component/codec/legacy"
	jconfig "go.minekube.com/gate/pkg/edition/java/config"
	"go.minekube.com/gate/pkg/edition/java/ping"
	"go.minekube.com/gate/pkg/edition/java/proxy"
	gproto "go.minekube.com/gate/pkg/gate/proto"
	"go.minekube.com/gate/pkg/util/netutil"

	"github.com/morinoparty/moripa-worlds/internal/apiclient"
	wworld "github.com/morinoparty/moripa-worlds/internal/world"
	"github.com/morinoparty/moripa-worlds/internal/worldid"
)

// Plugin は Gate のイベントを受けて、ロビーでの待機とワールドへの移動を行う。
type Plugin struct {
	Gatekeeper Gatekeeper
	API        API
	// Lobby はロビーのサーバー名 (Gate の設定の servers に登録してあること)。
	Lobby       string
	StartupWait time.Duration
	Log         *slog.Logger
	// Metrics は NewMetrics で作る (players_waiting は Waiter から取る)。
	Metrics *Metrics
	// MaxKickRedirects はワールドから落とされたときにロビーへ戻して待ち直す回数の上限 (既定 3)。
	MaxKickRedirects int
	// PingCacheTTL はサーバーリスト用の状態を覚えておく時間 (既定 5 秒)。
	PingCacheTTL time.Duration

	px     *proxy.Proxy
	waiter *Waiter
	cache  *stateCache

	mu    sync.Mutex
	kicks map[string]int
}

// GatePlugin は Gate に登録するプラグイン。
func (pl *Plugin) GatePlugin() proxy.Plugin {
	return proxy.Plugin{Name: "moripa-worlds", Init: pl.Init}
}

// Waiting は待っているプレイヤーの数 (players_waiting)。Init の前は 0。
func (pl *Plugin) Waiting() float64 {
	pl.mu.Lock()
	w := pl.waiter
	pl.mu.Unlock()
	if w == nil {
		return 0
	}
	return float64(w.Waiting())
}

// Init は Gate がプロキシを作った後、接続を受け付ける前に呼ぶ。
func (pl *Plugin) Init(ctx context.Context, px *proxy.Proxy) error {
	if pl.Log == nil {
		pl.Log = slog.Default()
	}
	if px.Server(pl.Lobby) == nil {
		return fmt.Errorf("lobby server %q is not registered (config.servers)", pl.Lobby)
	}
	cache := newStateCache(pl.API, nil)
	w := &Waiter{
		API:         pl.API,
		Servers:     gateServers{px: px},
		Lobby:       pl.Lobby,
		Log:         pl.Log.With("component", "wait"),
		Metrics:     pl.Metrics,
		StartupWait: pl.StartupWait,
		Context:     ctx,
		cache:       cache,
	}
	pl.mu.Lock()
	pl.px, pl.waiter, pl.cache = px, w, cache
	pl.kicks = map[string]int{}
	pl.mu.Unlock()

	mgr := px.Event()
	event.Subscribe(mgr, 0, pl.onPreLogin)
	event.Subscribe(mgr, 0, pl.onChooseInitialServer)
	event.Subscribe(mgr, 0, pl.onKicked)
	event.Subscribe(mgr, 0, pl.onPing)
	event.Subscribe(mgr, 0, pl.onDisconnect)
	go func() {
		<-ctx.Done()
		w.Close()
	}()
	go pl.cleanupLoop(ctx, 10*time.Minute)
	return nil
}

// onPreLogin は認証の前に、ホスト名とバージョンが合わない接続を断る (Mojang に問い合わせない)。
func (pl *Plugin) onPreLogin(e *proxy.PreLoginEvent) {
	c := e.Conn()
	vh := ""
	if a := c.VirtualHost(); a != nil {
		vh = a.String()
	}
	if _, deny := pl.Gatekeeper.Check(vh, int(c.Protocol())); deny != "" {
		if deny == MsgBadHost {
			pl.Metrics.login("denied_host")
		} else {
			pl.Metrics.login("denied_version")
		}
		pl.Log.Info("denied login", "user", e.Username(), "host", vh, "protocol", int(c.Protocol()), "reason", deny)
		e.Deny(redText(deny))
	}
}

// onChooseInitialServer は認証済みのプレイヤーの最初の接続先を決める。
// ここでだけワールドを作る (ensure)。同期イベントなので API 呼び出しは短いタイムアウトで済ませる。
func (pl *Plugin) onChooseInitialServer(e *proxy.PlayerChooseInitialServerEvent) {
	p := e.Player()
	vh := ""
	if a := p.VirtualHost(); a != nil {
		vh = a.String()
	}
	id, deny := pl.Gatekeeper.Check(vh, int(p.Protocol()))
	if deny != "" {
		// PreLogin で断っているので通常は来ない。
		e.SetInitialServer(nil)
		p.Disconnect(redText(deny))
		return
	}
	log := pl.Log.With("player", p.Username(), "id", id)
	d := DecideInitial(p.Context(), pl.API, id)
	if d.World != nil {
		pl.cache.Put(d.World)
	}
	pl.Metrics.login(d.Action.String())
	log.Info("initial server", "decision", d.Action.String(), "message", d.Message)
	switch d.Action {
	case ActConnectWorld:
		name := WorldServerName(id)
		err := gateServers{px: pl.px}.Ensure(name, d.World.Address)
		if err == nil {
			e.SetInitialServer(pl.px.Server(name))
			return
		}
		log.Error("failed to register world server; waiting in lobby instead", "error", err)
		fallthrough
	case ActLobbyWait:
		lobby := pl.px.Server(pl.Lobby)
		if lobby == nil {
			e.SetInitialServer(nil)
			p.Disconnect(redText(MsgError))
			return
		}
		e.SetInitialServer(lobby)
		pl.waiter.Start(gatePlayer{p: p, px: pl.px}, id, WaitOptions{World: d.World, Ensured: true})
	default:
		e.SetInitialServer(nil)
		p.Disconnect(redText(d.Message))
	}
}

// onKicked はワールドから落とされた (クラッシュ・再起動など) プレイヤーをロビーへ戻し、待ち直させる。
func (pl *Plugin) onKicked(e *proxy.KickedFromServerEvent) {
	id, ok := WorldIDFromServerName(e.Server().ServerInfo().Name())
	if !ok {
		return // ロビーからのキックは Gate の既定どおり (切断)
	}
	p := e.Player()
	gp := gatePlayer{p: p, px: pl.px}
	log := pl.Log.With("player", p.Username(), "id", id)
	reason := plainText(e.OriginalReason())
	if e.KickedDuringServerConnect() {
		// ロビーから移ろうとして失敗した。待機ループが数えて再試行するので、英語の既定文言だけ差し替える。
		if gp.CurrentServer() == pl.Lobby {
			e.SetResult(&proxy.NotifyKickResult{Message: yellowText(MsgConnectRetry)})
		}
		log.Info("could not connect to world", "reason", reason)
		return
	}
	if DeliberateKick(reason) {
		log.Info("kicked from world (deliberate)", "reason", reason)
		return
	}
	lobby := pl.px.Server(pl.Lobby)
	if lobby == nil {
		return
	}
	pl.mu.Lock()
	pl.kicks[gp.Key()]++
	n := pl.kicks[gp.Key()]
	pl.mu.Unlock()
	limit := pl.MaxKickRedirects
	if limit <= 0 {
		limit = 3
	}
	if n > limit {
		log.Info("kicked from world too many times", "reason", reason, "count", n)
		e.SetResult(&proxy.DisconnectPlayerKickResult{Reason: redText(MsgTooManyRetries)})
		return
	}
	log.Info("kicked from world; back to lobby", "reason", reason, "count", n)
	e.SetResult(&proxy.RedirectPlayerKickResult{Server: lobby, Message: yellowText(MsgWorldLost)})
	pl.cache.Invalidate(id)
	// 落ちた直後は Deployment がまだ Ready に見えることがあるので、少し置いてから移す。
	pl.waiter.Start(gp, id, WaitOptions{ConnectDelay: 5 * time.Second})
}

func (pl *Plugin) onDisconnect(e *proxy.DisconnectEvent) {
	key := e.Player().ID().String()
	pl.waiter.Stop(key)
	pl.mu.Lock()
	delete(pl.kicks, key)
	pl.mu.Unlock()
}

// onPing はサーバーリストの表示を作る。ping ではワールドを作らない。
func (pl *Plugin) onPing(e *proxy.PingEvent) {
	vh := ""
	if a := e.Connection().VirtualHost(); a != nil {
		vh = a.String()
	}
	id, err := worldid.FromVirtualHost(vh, pl.Gatekeeper.Suffix)
	valid := err == nil
	var (
		w      *apiclient.World
		apiErr error
	)
	online := 0
	if valid {
		ttl := pl.PingCacheTTL
		if ttl <= 0 {
			ttl = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(e.Connection().Context(), 3*time.Second)
		w, apiErr = pl.cache.Get(ctx, id, ttl)
		cancel()
		if rs := pl.px.Server(WorldServerName(id)); rs != nil {
			online = rs.Players().Len()
		}
	}
	motd := MOTD(valid, w, apiErr)
	desc, derr := (&legacy.Legacy{Char: legacy.SectionChar}).Unmarshal([]byte(motd))
	if derr != nil {
		desc = &component.Text{Content: motd}
	}
	favicon := e.Ping().Favicon
	if favicon == jconfig.DefaultConfig.Status.Favicon {
		favicon = "" // Gate のアイコンは出さない
	}
	e.SetPing(&ping.ServerPing{
		Version: ping.Version{
			Protocol: gproto.Protocol(pl.Gatekeeper.RequiredProtocol),
			Name:     pl.Gatekeeper.RequiredVersionName,
		},
		Players:     &ping.Players{Online: online, Max: wworld.MaxPlayers},
		Description: desc,
		Favicon:     favicon,
	})
}

// cleanupLoop は誰もいないワールドのサーバー登録を時々外す (登録が増え続けないように)。
func (pl *Plugin) cleanupLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, rs := range pl.px.Servers() {
			if _, ok := WorldIDFromServerName(rs.ServerInfo().Name()); ok && rs.Players().Len() == 0 {
				pl.px.Unregister(rs.ServerInfo())
			}
		}
	}
}

// gatePlayer は Gate の Player を Player にする。
type gatePlayer struct {
	p  proxy.Player
	px *proxy.Proxy
}

func (g gatePlayer) Key() string              { return g.p.ID().String() }
func (g gatePlayer) Name() string             { return g.p.Username() }
func (g gatePlayer) Context() context.Context { return g.p.Context() }

func (g gatePlayer) CurrentServer() string {
	if s := g.p.CurrentServer(); s != nil {
		return s.Server().ServerInfo().Name()
	}
	return ""
}

func (g gatePlayer) ActionBar(msg string) {
	_ = g.p.SendActionBar(&component.Text{Content: msg, S: component.Style{Color: color.Yellow}})
}

func (g gatePlayer) Disconnect(msg string) { g.p.Disconnect(redText(msg)) }

func (g gatePlayer) Connect(ctx context.Context, name string) bool {
	rs := g.px.Server(name)
	if rs == nil {
		return false
	}
	return g.p.CreateConnectionRequest(rs).ConnectWithIndication(ctx)
}

// gateServers はワールドのサーバーを Gate に登録する。
type gateServers struct{ px *proxy.Proxy }

func (s gateServers) Ensure(name, addr string) error {
	a, err := netutil.Parse(addr, "tcp")
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", addr, err)
	}
	info := proxy.NewServerInfo(name, a)
	for range 2 {
		rs, err := s.px.Register(info)
		if err == nil {
			return nil
		}
		if !errors.Is(err, proxy.ErrServerAlreadyExists) {
			return err
		}
		if proxy.ServerInfoEqual(rs.ServerInfo(), info) {
			return nil
		}
		// 別のアドレスで登録されていた (コントローラーの設定が変わった等)。入れ替える。
		s.px.Unregister(rs.ServerInfo())
	}
	return errors.New("server registration kept changing")
}

func redText(s string) component.Component {
	return &component.Text{Content: s, S: component.Style{Color: color.Red}}
}

func yellowText(s string) component.Component {
	return &component.Text{Content: s, S: component.Style{Color: color.Yellow}}
}

// plainText はキックの理由の本文と翻訳キーをつないだ文字列 (判定・ログ用)。
func plainText(c component.Component) string {
	var b strings.Builder
	var walk func(c component.Component, depth int)
	walk = func(c component.Component, depth int) {
		if c == nil || depth > 8 || b.Len() > 1024 {
			return
		}
		switch t := c.(type) {
		case *component.Text:
			b.WriteString(t.Content)
			for _, x := range t.Extra {
				walk(x, depth+1)
			}
		case *component.Translation:
			b.WriteString(" " + t.Key + " " + t.Fallback + " ")
			for _, x := range t.With {
				walk(x, depth+1)
			}
		}
	}
	walk(c, 0)
	return b.String()
}
