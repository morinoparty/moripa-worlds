package proxy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/morinoparty/moripa-worlds/internal/apiclient"
)

const (
	suffix = ".world.site1.dev.morino.party"
	wid    = "0199c8a0-7b3e-7f12-9a4c-1d2e3f405162"
)

func TestGatekeeper(t *testing.T) {
	g := Gatekeeper{Suffix: suffix, RequiredProtocol: 777, RequiredVersionName: "26.3"}
	tests := []struct {
		name     string
		host     string
		protocol int
		wantID   string
		wantDeny string
	}{
		{"ok", wid + suffix + ":25565", 777, wid, ""},
		{"ok uppercase trailing dot", strings.ToUpper(wid+suffix) + ".:25565", 777, wid, ""},
		{"ok forge", wid + suffix + "\x00FML3\x00:25565", 777, wid, ""},
		{"old client", wid + suffix + ":25565", 776, "", "Minecraft 26.3 で接続してください"},
		{"new client", wid + suffix + ":25565", 778, "", "Minecraft 26.3 で接続してください"},
		{"bad host beats version", "play.morino.party:25565", 776, "", MsgBadHost},
		{"bad id", "nope" + suffix + ":25565", 777, "", MsgBadHost},
		{"uuid v4", "069a79f4-44e9-4726-a5be-fca90e38aaf5" + suffix + ":25565", 777, "", MsgBadHost},
		{"empty", "", 777, "", MsgBadHost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, deny := g.Check(tt.host, tt.protocol)
			if id != tt.wantID || deny != tt.wantDeny {
				t.Fatalf("Check(%q, %d) = %q, %q; want %q, %q", tt.host, tt.protocol, id, deny, tt.wantID, tt.wantDeny)
			}
		})
	}
}

// fakeAPI は状態と ensure の結果を差し替えられる API。
type fakeAPI struct {
	mu         sync.Mutex
	state      apiclient.World
	stateErr   error
	ensure     apiclient.World
	ensureErr  error
	states     int
	ensures    int
	onEnsure   func(*fakeAPI)
	stateBlock chan struct{}
}

func (f *fakeAPI) State(ctx context.Context, id string) (*apiclient.World, error) {
	f.mu.Lock()
	f.states++
	block := f.stateBlock
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateErr != nil {
		return nil, f.stateErr
	}
	w := f.state
	w.ID = id
	return &w, nil
}

func (f *fakeAPI) Ensure(_ context.Context, id string) (*apiclient.World, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures++
	if f.onEnsure != nil {
		f.onEnsure(f)
	}
	if f.ensureErr != nil {
		return nil, f.ensureErr
	}
	w := f.ensure
	w.ID = id
	return &w, nil
}

func (f *fakeAPI) set(fn func(*fakeAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeAPI) counts() (states, ensures int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states, f.ensures
}

func world(found bool, state apiclient.State, result apiclient.Result) apiclient.World {
	title := "テストワールド"
	return apiclient.World{Found: found, Title: &title, State: state, Result: result, Address: "w-" + wid + ".worlds.svc.cluster.local:25565"}
}

func TestDecideInitial(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name        string
		api         *fakeAPI
		want        Action
		wantMsg     string
		wantEnsures int
	}{
		{"state error", &fakeAPI{stateErr: boom}, ActDisconnect, MsgError, 0},
		{"not found", &fakeAPI{state: world(false, apiclient.StateAbsent, "")}, ActDisconnect, "ワールドが見つかりません: " + wid, 0},
		{"ready", &fakeAPI{state: world(true, apiclient.StateReady, "")}, ActConnectWorld, "", 0},
		{"absent created", &fakeAPI{state: world(true, apiclient.StateAbsent, ""), ensure: world(true, apiclient.StateStarting, apiclient.ResultCreated)}, ActLobbyWait, "", 1},
		{"starting existing", &fakeAPI{state: world(true, apiclient.StateStarting, ""), ensure: world(true, apiclient.StateStarting, apiclient.ResultExisting)}, ActLobbyWait, "", 1},
		{"became ready while ensuring", &fakeAPI{state: world(true, apiclient.StateStarting, ""), ensure: world(true, apiclient.StateReady, apiclient.ResultExisting)}, ActConnectWorld, "", 1},
		{"full", &fakeAPI{state: world(true, apiclient.StateAbsent, ""), ensure: world(true, apiclient.StateAbsent, apiclient.ResultFull)}, ActDisconnect, MsgBusy, 1},
		{"rate limited", &fakeAPI{state: world(true, apiclient.StateAbsent, ""), ensure: world(true, apiclient.StateAbsent, apiclient.ResultRateLimited)}, ActDisconnect, MsgBusy, 1},
		{"ensure result error", &fakeAPI{state: world(true, apiclient.StateAbsent, ""), ensure: world(true, apiclient.StateAbsent, apiclient.ResultError)}, ActDisconnect, MsgError, 1},
		{"ensure transport error", &fakeAPI{state: world(true, apiclient.StateAbsent, ""), ensureErr: boom}, ActDisconnect, MsgError, 1},
		{"deleted between calls", &fakeAPI{state: world(true, apiclient.StateAbsent, ""), ensure: world(false, apiclient.StateAbsent, apiclient.ResultNotFound)}, ActDisconnect, "ワールドが見つかりません: " + wid, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DecideInitial(context.Background(), tt.api, wid)
			if d.Action != tt.want || d.Message != tt.wantMsg {
				t.Fatalf("DecideInitial = %v %q; want %v %q", d.Action, d.Message, tt.want, tt.wantMsg)
			}
			if _, e := tt.api.counts(); e != tt.wantEnsures {
				t.Fatalf("ensures = %d, want %d", e, tt.wantEnsures)
			}
			if d.Action == ActConnectWorld && d.World.Address == "" {
				t.Fatal("no address")
			}
		})
	}
}

func TestDeliberateKick(t *testing.T) {
	for _, r := range []string{
		"You are banned from this server", " multiplayer.disconnect.not_whitelisted  ", "Kicked by an operator",
		"あなたはこのサーバーから追放されました", "You logged in from another location", "multiplayer.disconnect.duplicate_login",
	} {
		if !DeliberateKick(r) {
			t.Errorf("DeliberateKick(%q) = false", r)
		}
	}
	for _, r := range []string{"", "An internal error occurred in your connection.", "Server closed", "Timed out", "サーバーを停止しています"} {
		if DeliberateKick(r) {
			t.Errorf("DeliberateKick(%q) = true", r)
		}
	}
}

func TestMOTD(t *testing.T) {
	ready, starting, absent, missing := world(true, apiclient.StateReady, ""), world(true, apiclient.StateStarting, ""), world(true, apiclient.StateAbsent, ""), world(false, apiclient.StateAbsent, "")
	evil := "§k悪い\nタイトル"
	evilW := world(true, apiclient.StateReady, "")
	evilW.Title = &evil
	noTitle := world(true, apiclient.StateAbsent, "")
	noTitle.Title = nil
	tests := []struct {
		name  string
		valid bool
		w     *apiclient.World
		err   error
		want  string
	}{
		{"ready", true, &ready, nil, "テストワールド\n§7プレイ可能"},
		{"starting", true, &starting, nil, "テストワールド\n§e起動中…"},
		{"absent", true, &absent, nil, "テストワールド\n§7参加すると起動します"},
		{"not found", true, &missing, nil, "§cワールドが見つかりません"},
		{"invalid host", false, nil, nil, "§cこのアドレスでは接続できません"},
		{"api error", true, nil, errors.New("down"), "§c状態を確認できません"},
		{"title sanitized", true, &evilW, nil, "k悪い タイトル\n§7プレイ可能"},
		{"default title", true, &noTitle, nil, "morino.party world\n§7参加すると起動します"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MOTD(tt.valid, tt.w, tt.err); got != tt.want {
				t.Fatalf("MOTD = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorldServerName(t *testing.T) {
	if id, ok := WorldIDFromServerName(WorldServerName(wid)); !ok || id != wid {
		t.Fatalf("round trip = %q %v", id, ok)
	}
	for _, n := range []string{"lobby", "w-", "w-nope", "x-" + wid} {
		if _, ok := WorldIDFromServerName(n); ok {
			t.Errorf("WorldIDFromServerName(%q) = ok", n)
		}
	}
	if len(WorldServerName(wid)) > 63 {
		t.Fatal("server name too long for Gate")
	}
}
