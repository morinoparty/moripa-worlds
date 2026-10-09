// Package waker は Gate Lite から転送されてきた Minecraft の接続を受け、
// ログインなら対象のワールドを起動 (Kubernetes に作成) して再接続を促す切断メッセージを返し、
// サーバーリストの ping には「停止中 (参加すると起動)」または「起動中」の MOTD を返す。
//
// ping ではワールドを作らない。ping は認証なしで送れるため、ping で作ると
// 参加されないワールドで MAX_WORLDS の枠を埋め続けられてしまう。
//
// Gate はワールド w-<uuid> に接続できなかったとき (未起動・起動中) だけこの waker に接続を回す。
package waker

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/morinoparty/moripa-worlds/internal/mcproto"
	"github.com/morinoparty/moripa-worlds/internal/metrics"
	"github.com/morinoparty/moripa-worlds/internal/schematic"
	"github.com/morinoparty/moripa-worlds/internal/world"
	"github.com/morinoparty/moripa-worlds/internal/worldid"
)

// プレイヤーに見せるメッセージ。
const (
	MsgNotFound      = "ワールドが見つかりません"
	MsgBusy          = "現在混み合っています。しばらくしてから再接続してください"
	MsgStartingLogin = "ワールドを起動しています。10〜30秒後に再接続してください"
	MsgSleeping      = "停止中です。参加すると起動します"
	MsgError         = "ワールドを起動できませんでした。しばらくしてから再接続してください"
)

// パケットサイズの上限。
const (
	MaxHandshakeLen     = 1024
	MaxStatusRequestLen = 16
	MaxPingLen          = 16
	// MaxLoginStartLen は Login Start の上限。1.19 系は署名鍵を含むため大きめにする。
	MaxLoginStartLen = 4096
)

// InfoStore は info.json を (キャッシュ経由で) 取得する。
type InfoStore interface {
	Get(ctx context.Context, id string) (*schematic.Info, error)
}

// WorldEnsurer はワールドを (なければ) 作る。
type WorldEnsurer interface {
	Ensure(ctx context.Context, info *schematic.Info) (world.EnsureResult, error)
	// Exists はワールドが既にある (起動中) かをキャッシュから返す。API を叩かないこと。
	Exists(id string) bool
}

// Server は waker の TCP サーバー。
type Server struct {
	HostSuffix string
	Store      InfoStore
	Worlds     WorldEnsurer
	Log        *slog.Logger
	Metrics    *metrics.Metrics
	Now        func() time.Time
	// ReadTimeout は各パケットを読むまでの上限 (既定 5 秒)。
	ReadTimeout time.Duration
	// MaxConns は同時に処理する接続数の上限 (既定 256)。超えた接続はすぐに閉じる。
	MaxConns int

	wg sync.WaitGroup
}

func (s *Server) readTimeout() time.Duration {
	if s.ReadTimeout > 0 {
		return s.ReadTimeout
	}
	return 5 * time.Second
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Serve は ln で接続を受け付ける。ctx が終わるとリスナーを閉じ、処理中の接続を待ってから戻る。
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	maxConns := s.MaxConns
	if maxConns <= 0 {
		maxConns = 256
	}
	sem := make(chan struct{}, maxConns)
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	defer s.wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			s.Log.Warn("too many concurrent connections; dropping", "remote", conn.RemoteAddr().String())
			_ = conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-sem }()
			s.Handle(ctx, conn)
		}()
	}
}

// outcome はワールドの解決結果。
type outcome struct {
	result string // metrics の result ラベル
	id     string
	label  string // 見つからない場合に表示する ID 部分
	info   *schematic.Info
}

// Handle は 1 つの接続を処理して閉じる。パニックはしない。
func (s *Server) Handle(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("panic while handling connection", "panic", r)
		}
	}()
	log := s.Log.With("remote", conn.RemoteAddr().String())
	br := bufio.NewReaderSize(conn, 512)

	read := func(max int) (mcproto.Packet, error) {
		_ = conn.SetReadDeadline(s.now().Add(s.readTimeout()))
		return mcproto.ReadPacket(br, max)
	}
	write := func(id int32, data []byte) error {
		_ = conn.SetWriteDeadline(s.now().Add(s.readTimeout()))
		return mcproto.WritePacket(conn, id, data)
	}

	p, err := read(MaxHandshakeLen)
	if err != nil {
		s.Metrics.Connection("unknown", "bad_request")
		log.Debug("failed to read handshake", "error", err)
		return
	}
	hs, err := mcproto.ParseHandshake(p)
	if err != nil {
		s.Metrics.Connection("unknown", "bad_request")
		log.Debug("invalid handshake", "error", err)
		return
	}
	state := "status"
	if hs.IsLogin() {
		state = "login"
	}
	log = log.With("state", state, "host", hs.ServerAddress, "protocol", hs.ProtocolVersion)

	if !hs.IsLogin() {
		p, err := read(MaxStatusRequestLen)
		if err != nil || p.ID != mcproto.StatusRequestID || len(p.Data) != 0 {
			s.Metrics.Connection(state, "bad_request")
			log.Debug("invalid status request", "error", err)
			return
		}
	} else {
		// Login Start の中身 (バージョンごとに形が違う) は使わないので読み捨てる。
		if _, err := read(MaxLoginStartLen); err != nil {
			log.Debug("failed to read login start", "error", err)
		}
	}

	o := s.resolve(ctx, hs.ServerAddress, hs.IsLogin(), log)
	s.Metrics.Connection(state, o.result)
	log.Info("handled connection", "id", o.id, "result", o.result)

	if hs.IsLogin() {
		_ = write(mcproto.LoginDisconnectID, mcproto.EncodeString(mcproto.TextJSON(loginMessage(o))))
		return
	}
	resp := statusResponse(o, hs.ProtocolVersion)
	if err := write(mcproto.StatusResponseID, mcproto.EncodeString(resp.JSON())); err != nil {
		return
	}
	// Gate は ping/pong を自分で返すのでここには来ないが、直接接続のクライアントのために応答する。
	p, err = read(MaxPingLen)
	if err != nil {
		return
	}
	if v, err := mcproto.ParsePing(p, mcproto.PingRequestID); err == nil {
		_ = write(mcproto.PongResponseID, mcproto.EncodeLong(v))
	}
}

// resolve はホスト名からワールドを探す。create (ログイン) のときだけ、なければ作成する。
func (s *Server) resolve(ctx context.Context, addr string, create bool, log *slog.Logger) outcome {
	id, err := worldid.FromHost(addr, s.HostSuffix)
	if err != nil {
		label := worldid.CleanHost(addr)
		if l, ok := strings.CutSuffix(label, s.HostSuffix); ok && l != "" {
			label = l
		}
		return outcome{result: "not_found", label: truncate(label, 64)}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := s.Store.Get(ctx, id)
	if errors.Is(err, schematic.ErrNotFound) {
		return outcome{result: "not_found", id: id, label: id}
	}
	if err != nil {
		log.Error("failed to look up schematic", "id", id, "error", err)
		return outcome{result: "error", id: id, label: id}
	}
	if info.Expired(s.now()) {
		return outcome{result: "not_found", id: id, label: id}
	}
	if !create {
		// ステータス ping では作らない (認証なしで枠を埋められないように)。
		if s.Worlds.Exists(id) {
			return outcome{result: "starting", id: id, label: id, info: info}
		}
		return outcome{result: "sleeping", id: id, label: id, info: info}
	}
	res, err := s.Worlds.Ensure(ctx, info)
	if err != nil {
		log.Error("failed to ensure world", "id", id, "error", err)
		return outcome{result: "error", id: id, label: id, info: info}
	}
	switch res {
	case world.Full, world.RateLimited:
		return outcome{result: "busy", id: id, label: id, info: info}
	case world.Created:
		return outcome{result: "created", id: id, label: id, info: info}
	default:
		return outcome{result: "starting", id: id, label: id, info: info}
	}
}

func loginMessage(o outcome) mcproto.Text {
	switch o.result {
	case "created", "starting":
		return mcproto.Text{Text: MsgStartingLogin, Color: "green"}
	case "busy":
		return mcproto.Text{Text: MsgBusy, Color: "gold"}
	case "error":
		return mcproto.Text{Text: MsgError, Color: "red"}
	default:
		return mcproto.Text{Text: MsgNotFound + ": " + o.label, Color: "red"}
	}
}

func statusResponse(o outcome, protocol int32) mcproto.StatusResponse {
	r := mcproto.StatusResponse{
		// クライアントに「互換あり」と表示させるため、プロトコル番号はハンドシェイクのものを返す。
		Version: mcproto.StatusVersion{Name: "starting", Protocol: protocol},
		Players: mcproto.StatusPlayers{Max: world.MaxPlayers, Online: 0},
	}
	switch o.result {
	case "created", "starting":
		r.Description = mcproto.Text{Text: sprintfTitle(o.info.DisplayTitle())}
	case "sleeping":
		r.Version.Name = "sleeping"
		// 2 行の MOTD: 1 行目にタイトル、2 行目に案内。
		r.Description = mcproto.Text{Text: o.info.DisplayTitle() + "\n" + MsgSleeping}
	case "busy":
		r.Version.Name = "busy"
		r.Description = mcproto.Text{Text: MsgBusy, Color: "gold"}
	case "error":
		r.Version.Name = "error"
		r.Description = mcproto.Text{Text: MsgError, Color: "red"}
	default:
		r.Version.Name = "not found"
		r.Description = mcproto.Text{Text: MsgNotFound, Color: "red"}
	}
	return r
}

func sprintfTitle(title string) string {
	return title + " を起動しています…"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
