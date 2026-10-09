package waker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/morinoparty/moripa-worlds/internal/mcproto"
	"github.com/morinoparty/moripa-worlds/internal/schematic"
	"github.com/morinoparty/moripa-worlds/internal/world"
)

const (
	suffix  = ".world.site1.dev.morino.party"
	knownID = "0199c8a0-7b3e-7f12-9a4c-1d2e3f405162"
	otherID = "0199c8a0-7b3e-7f12-9a4c-ffffffffffff"
)

type fakeStore struct {
	infos map[string]*schematic.Info
	err   error
}

func (f *fakeStore) Get(_ context.Context, id string) (*schematic.Info, error) {
	if f.err != nil {
		return nil, f.err
	}
	if i, ok := f.infos[id]; ok {
		return i, nil
	}
	return nil, schematic.ErrNotFound
}

type fakeWorlds struct {
	mu     sync.Mutex
	calls  []string
	result world.EnsureResult
	err    error
}

func (f *fakeWorlds) Ensure(_ context.Context, info *schematic.Info) (world.EnsureResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, info.ID)
	return f.result, f.err
}

func (f *fakeWorlds) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newServer(worlds *fakeWorlds, store *fakeStore) *Server {
	return &Server{
		HostSuffix:  suffix,
		Store:       store,
		Worlds:      worlds,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReadTimeout: 2 * time.Second,
	}
}

func knownStore() *fakeStore {
	title := "ロビーの噴水"
	return &fakeStore{infos: map[string]*schematic.Info{
		knownID: {ID: knownID, Format: "sponge_v3", FileName: "schematic.schem", Title: &title},
	}}
}

// startPipe は net.Pipe の片側でサーバーの Handle を動かし、クライアント側を返す。
func startPipe(t *testing.T, s *Server) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() { s.Handle(context.Background(), server); close(done) }()
	t.Cleanup(func() {
		_ = client.Close()
		<-done
	})
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	return client
}

func send(t *testing.T, c net.Conn, id int32, data []byte) {
	t.Helper()
	if err := mcproto.WritePacket(c, id, data); err != nil {
		t.Fatal(err)
	}
}

func handshake(host string, next int32) []byte {
	return mcproto.Handshake{ProtocolVersion: 773, ServerAddress: host, ServerPort: 25565, NextState: next}.Encode()
}

func statusOf(t *testing.T, s *Server, host string) mcproto.StatusResponse {
	t.Helper()
	c := startPipe(t, s)
	br := bufio.NewReader(c)
	send(t, c, mcproto.HandshakeID, handshake(host, mcproto.NextStateStatus))
	send(t, c, mcproto.StatusRequestID, nil)
	p, err := mcproto.ReadPacket(br, mcproto.MaxStatusResponseLen)
	if err != nil {
		t.Fatal(err)
	}
	js, err := mcproto.ParseString(p, mcproto.StatusResponseID, mcproto.MaxStatusResponseLen)
	if err != nil {
		t.Fatal(err)
	}
	var resp mcproto.StatusResponse
	if err := json.Unmarshal([]byte(js), &resp); err != nil {
		t.Fatalf("status json %q: %v", js, err)
	}

	// ping / pong
	send(t, c, mcproto.PingRequestID, mcproto.EncodeLong(424242))
	p, err = mcproto.ReadPacket(br, 64)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := mcproto.ParsePing(p, mcproto.PongResponseID); err != nil || v != 424242 {
		t.Fatalf("pong = %d, %v", v, err)
	}
	return resp
}

func loginOf(t *testing.T, s *Server, host string, next int32) mcproto.Text {
	t.Helper()
	c := startPipe(t, s)
	br := bufio.NewReader(c)
	send(t, c, mcproto.HandshakeID, handshake(host, next))
	// Login Start (1.20.2+: name + uuid)
	send(t, c, mcproto.LoginStartID, append(mcproto.EncodeString("Steve"), make([]byte, 16)...))
	p, err := mcproto.ReadPacket(br, 4096)
	if err != nil {
		t.Fatal(err)
	}
	js, err := mcproto.ParseString(p, mcproto.LoginDisconnectID, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var txt mcproto.Text
	if err := json.Unmarshal([]byte(js), &txt); err != nil {
		t.Fatalf("disconnect json %q: %v", js, err)
	}
	// サーバーは切断後に接続を閉じる。
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after disconnect, got %v", err)
	}
	return txt
}

func TestStatusStartsWorld(t *testing.T) {
	w := &fakeWorlds{result: world.Created}
	s := newServer(w, knownStore())
	resp := statusOf(t, s, strings.ToUpper(knownID)+suffix+"\x00FML3\x00")
	if resp.Description.Text != "ロビーの噴水 を起動しています…" {
		t.Errorf("motd = %q", resp.Description.Text)
	}
	if resp.Version.Name != "starting" || resp.Version.Protocol != 773 {
		t.Errorf("version = %+v", resp.Version)
	}
	if resp.Players.Max != 20 || resp.Players.Online != 0 {
		t.Errorf("players = %+v", resp.Players)
	}
	if got := w.Calls(); len(got) != 1 || got[0] != knownID {
		t.Errorf("ensure calls = %v", got)
	}
}

func TestStatusUnknownWorld(t *testing.T) {
	w := &fakeWorlds{result: world.Created}
	s := newServer(w, knownStore())
	for _, host := range []string{
		otherID + suffix, // info.json がない
		"069a79f4-44e9-4726-a5be-fca90e38aaf5" + suffix, // UUID v4
		knownID + ".world.example.com",                  // サフィックス違い
		"not-a-uuid" + suffix,
	} {
		resp := statusOf(t, s, host)
		if resp.Description.Text != MsgNotFound || resp.Description.Color != "red" {
			t.Errorf("%s: motd = %+v", host, resp.Description)
		}
	}
	if got := w.Calls(); len(got) != 0 {
		t.Errorf("ensure must not be called: %v", got)
	}
}

func TestStatusExpiredWorld(t *testing.T) {
	store := knownStore()
	past := time.Now().Add(-time.Minute)
	store.infos[knownID].DeleteAt = &past
	w := &fakeWorlds{result: world.Created}
	resp := statusOf(t, newServer(w, store), knownID+suffix)
	if resp.Description.Text != MsgNotFound {
		t.Errorf("motd = %q", resp.Description.Text)
	}
	if len(w.Calls()) != 0 {
		t.Error("ensure called for expired world")
	}
}

func TestStatusBusy(t *testing.T) {
	for _, res := range []world.EnsureResult{world.Full, world.RateLimited} {
		resp := statusOf(t, newServer(&fakeWorlds{result: res}, knownStore()), knownID+suffix)
		if resp.Description.Text != MsgBusy {
			t.Errorf("%v: motd = %q", res, resp.Description.Text)
		}
	}
}

func TestLoginStartsWorld(t *testing.T) {
	for _, next := range []int32{mcproto.NextStateLogin, mcproto.NextStateTransfer} {
		w := &fakeWorlds{result: world.Existing}
		txt := loginOf(t, newServer(w, knownStore()), knownID+suffix+".", next)
		if txt.Text != MsgStartingLogin {
			t.Errorf("next=%d: disconnect = %q", next, txt.Text)
		}
		if len(w.Calls()) != 1 {
			t.Errorf("next=%d: ensure calls = %v", next, w.Calls())
		}
	}
}

func TestLoginUnknownWorld(t *testing.T) {
	txt := loginOf(t, newServer(&fakeWorlds{}, knownStore()), otherID+suffix, mcproto.NextStateLogin)
	if txt.Text != MsgNotFound+": "+otherID || txt.Color != "red" {
		t.Errorf("disconnect = %+v", txt)
	}
}

func TestLoginBusyAndError(t *testing.T) {
	txt := loginOf(t, newServer(&fakeWorlds{result: world.Full}, knownStore()), knownID+suffix, mcproto.NextStateLogin)
	if txt.Text != MsgBusy {
		t.Errorf("busy disconnect = %q", txt.Text)
	}
	txt = loginOf(t, newServer(&fakeWorlds{err: errors.New("k8s down")}, knownStore()), knownID+suffix, mcproto.NextStateLogin)
	if txt.Text != MsgError {
		t.Errorf("error disconnect = %q", txt.Text)
	}
	txt = loginOf(t, newServer(&fakeWorlds{}, &fakeStore{err: errors.New("s3 down")}), knownID+suffix, mcproto.NextStateLogin)
	if txt.Text != MsgError {
		t.Errorf("s3 error disconnect = %q", txt.Text)
	}
}

// TestMalformedInputs は不正な入力で接続が閉じられ、パニックしないことを確かめる。
func TestMalformedInputs(t *testing.T) {
	inputs := map[string][]byte{
		"oversized handshake": mcproto.AppendVarInt(nil, 2048),
		"wrong packet id":     mcproto.AppendPacket(nil, 0x07, handshake(knownID+suffix, 1)),
		"truncated":           mcproto.AppendPacket(nil, 0, handshake(knownID+suffix, 1))[:10],
		"legacy ping":         {0xfe, 0x01, 0xfa, 0x00, 0x0b},
		"bad next state":      mcproto.AppendPacket(nil, 0, handshake(knownID+suffix, 7)),
		"status request with payload": append(mcproto.AppendPacket(nil, 0, handshake(knownID+suffix, 1)),
			mcproto.AppendPacket(nil, 0, []byte{1, 2, 3})...),
	}
	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			w := &fakeWorlds{result: world.Created}
			s := newServer(w, knownStore())
			s.ReadTimeout = 200 * time.Millisecond
			c := startPipe(t, s)
			go func() { _, _ = c.Write(in) }()
			buf := make([]byte, 1)
			if _, err := c.Read(buf); err == nil {
				t.Fatal("expected the server to close without a response")
			}
			if len(w.Calls()) != 0 {
				t.Fatal("ensure must not be called")
			}
		})
	}
}

func TestSlowClientTimesOut(t *testing.T) {
	s := newServer(&fakeWorlds{}, knownStore())
	s.ReadTimeout = 100 * time.Millisecond
	c := startPipe(t, s)
	start := time.Now()
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err == nil {
		t.Fatal("expected close")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("read deadline not applied")
	}
}

// TestServeRealListener は実際の TCP リスナーで status と login を通す。
func TestServeRealListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w := &fakeWorlds{result: world.Created}
	s := newServer(w, knownStore())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	// mcproto.Ping (reaper が使うクライアント) で status を取る。
	// ping のホスト名は addr のホスト部分なので、名前解決できる形でサフィックス付きの名前は使えない。
	// ここでは直接ハンドシェイクを書く。
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	send(t, conn, mcproto.HandshakeID, handshake(knownID+suffix, mcproto.NextStateStatus))
	send(t, conn, mcproto.StatusRequestID, nil)
	p, err := mcproto.ReadPacket(bufio.NewReader(conn), mcproto.MaxStatusResponseLen)
	if err != nil {
		t.Fatal(err)
	}
	js, _ := mcproto.ParseString(p, 0, mcproto.MaxStatusResponseLen)
	if !strings.Contains(js, "を起動しています") {
		t.Fatalf("status = %s", js)
	}
	_ = conn.Close()

	// mcproto.Ping はホスト 127.0.0.1 で接続するので「見つかりません」になるが、プロトコルとしては成功する。
	r, err := mcproto.Ping(context.Background(), ln.Addr().String(), 2*time.Second)
	if err != nil || r.Max != 20 || r.Online != 0 {
		t.Fatalf("Ping = %+v, %v", r, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}
