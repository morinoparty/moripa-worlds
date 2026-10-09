// Package proxy は moripa-worlds proxy (Gate を組み込んだ Minecraft プロキシ) のプラグイン。
//
// プレイヤーは <uuid>.world.site1.dev.morino.party に接続し、Mojang の認証を済ませると
// ワールド w-<uuid> が起動済みならそこへ、まだならロビーへ入る。ロビーにいる間に
// ワールドの起動を待ち (コントローラーの API を 1 秒ごとに見る)、起動したらそのまま移す。
//
// このファイルには Gate に依存しない判断 (ホスト名・バージョンの確認、最初の接続先の決定、
// キックの扱い、MOTD) を置く。Gate との配線は plugin.go。
package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/morinoparty/moripa-worlds/internal/apiclient"
	"github.com/morinoparty/moripa-worlds/internal/worldid"
)

// プレイヤーに見せるメッセージ。
const (
	MsgBadHost        = "このアドレスでは接続できません"
	MsgNotFound       = "ワールドが見つかりません"
	MsgBusy           = "現在混み合っています。しばらくしてから再接続してください"
	MsgError          = "ワールドを起動できませんでした。しばらくしてから再接続してください"
	MsgStartTimeout   = "ワールドの起動に時間が掛かりすぎています。しばらくしてから再接続してください"
	MsgConnectFailed  = "ワールドに接続できませんでした。しばらくしてから再接続してください"
	MsgConnectRetry   = "ワールドへの接続に失敗しました。再試行しています…"
	MsgWorldLost      = "ワールドとの接続が切れました。再起動を待っています…"
	MsgTooManyRetries = "ワールドとの接続が繰り返し切れたため切断しました。しばらくしてから再接続してください"
	MsgStatusUnknown  = "状態を確認できません"
)

// MsgVersion は対応していないバージョンで接続したときのメッセージ。
func MsgVersion(versionName string) string {
	return "Minecraft " + versionName + " で接続してください"
}

// MsgNotFoundID は見つからないワールドの ID 付きのメッセージ。
func MsgNotFoundID(id string) string {
	return MsgNotFound + ": " + id
}

// MsgStarting はロビーで待っている間のアクションバー。
func MsgStarting(title string, seconds int) string {
	return fmt.Sprintf("「%s」を起動しています… %d秒", title, seconds)
}

// API はコントローラーの API。*apiclient.Client が満たす。
type API interface {
	State(ctx context.Context, id string) (*apiclient.World, error)
	Ensure(ctx context.Context, id string) (*apiclient.World, error)
}

// Gatekeeper はログインの前 (認証の前) に、ホスト名とバージョンを確かめる。
type Gatekeeper struct {
	// Suffix は ".world.site1.dev.morino.party" のように先頭 "." 付き。
	Suffix              string
	RequiredProtocol    int
	RequiredVersionName string
}

// Check は接続先のワールド ID を返す。受け付けない場合は deny に切断メッセージを返す。
// virtualHost は Gate の VirtualHost().String() ("<host>:<port>")。
// ホスト名を先に確かめる (どのワールドでもない接続にバージョンの案内は要らない)。
func (g Gatekeeper) Check(virtualHost string, protocol int) (id, deny string) {
	id, err := worldid.FromVirtualHost(virtualHost, g.Suffix)
	if err != nil {
		return "", MsgBadHost
	}
	if protocol != g.RequiredProtocol {
		return "", MsgVersion(g.RequiredVersionName)
	}
	return id, ""
}

// Action は最初の接続先の決定。
type Action int

const (
	// ActDisconnect は切断する (Message を表示)。
	ActDisconnect Action = iota
	// ActConnectWorld は起動済みのワールドへ直接入れる。
	ActConnectWorld
	// ActLobbyWait はロビーへ入れて、ワールドの起動を待つ。
	ActLobbyWait
)

func (a Action) String() string {
	switch a {
	case ActDisconnect:
		return "disconnect"
	case ActConnectWorld:
		return "world"
	case ActLobbyWait:
		return "lobby"
	}
	return fmt.Sprintf("Action(%d)", int(a))
}

// Decision は DecideInitial の結果。
type Decision struct {
	Action  Action
	Message string
	World   *apiclient.World
}

// DecideInitial は Mojang の認証を終えたプレイヤーの最初の接続先を決める。
// ワールドを作る (ensure) のはここだけ (認証済みのプレイヤーが来たとき)。
// api の呼び出しにはそれぞれ短いタイムアウトが掛かっている (apiclient.Client.Timeout)。
func DecideInitial(ctx context.Context, api API, id string) Decision {
	w, err := api.State(ctx, id)
	if err != nil {
		return Decision{Action: ActDisconnect, Message: MsgError}
	}
	if !w.Found {
		return Decision{Action: ActDisconnect, Message: MsgNotFoundID(id), World: w}
	}
	if w.State == apiclient.StateReady {
		return Decision{Action: ActConnectWorld, World: w}
	}
	e, err := api.Ensure(ctx, id)
	if err != nil {
		return Decision{Action: ActDisconnect, Message: MsgError, World: w}
	}
	return decideEnsure(e)
}

// decideEnsure は ensure の結果を判断にする。
func decideEnsure(e *apiclient.World) Decision {
	switch e.Result {
	case apiclient.ResultCreated, apiclient.ResultExisting:
		if e.State == apiclient.StateReady {
			return Decision{Action: ActConnectWorld, World: e}
		}
		return Decision{Action: ActLobbyWait, World: e}
	case apiclient.ResultNotFound:
		return Decision{Action: ActDisconnect, Message: MsgNotFoundID(e.ID), World: e}
	case apiclient.ResultFull, apiclient.ResultRateLimited:
		return Decision{Action: ActDisconnect, Message: MsgBusy, World: e}
	default:
		return Decision{Action: ActDisconnect, Message: MsgError, World: e}
	}
}

// WorldServerName はワールドの Gate 上のサーバー名 (w-<uuid>)。
func WorldServerName(id string) string { return "w-" + id }

// WorldIDFromServerName は w-<uuid> から ID を返す。ワールドでなければ false。
func WorldIDFromServerName(name string) (string, bool) {
	id, ok := strings.CutPrefix(strings.ToLower(name), "w-")
	if !ok || !worldid.Valid(id) {
		return "", false
	}
	return id, true
}

// deliberateKickMarkers は「わざと」追い出されたことを示す語 (小文字)。
// これらはワールドの停止ではないので、ロビーへ戻して起動を待ったりはしない。
var deliberateKickMarkers = []string{
	"multiplayer.disconnect.banned",
	"multiplayer.disconnect.ip_banned",
	"multiplayer.disconnect.not_whitelisted",
	"multiplayer.disconnect.kicked",
	"multiplayer.disconnect.duplicate_login",
	"banned",
	"ban:",
	"whitelist",
	"white-list",
	"white list",
	"kicked by an operator",
	"logged in from another location",
	"追放",
	"ホワイトリスト",
	"bannされ",
	"ban されています",
}

// DeliberateKick はキックの理由 (平文とキー) が BAN・ホワイトリスト・管理者によるキックなど、
// 意図した切断に見えるかを返す。
func DeliberateKick(reason string) bool {
	r := strings.ToLower(reason)
	for _, m := range deliberateKickMarkers {
		if strings.Contains(r, m) {
			return true
		}
	}
	return false
}

// MOTD はサーバーリストに出す説明 (§ の書式コード付き) を返す。
// valid=false はホスト名がワールドのものでない場合。
func MOTD(valid bool, w *apiclient.World, err error) string {
	switch {
	case !valid:
		return "§c" + MsgBadHost
	case err != nil || w == nil:
		return "§c" + MsgStatusUnknown
	case !w.Found:
		return "§c" + MsgNotFound
	}
	title := sanitizeTitle(w.DisplayTitle())
	switch w.State {
	case apiclient.StateReady:
		return title + "\n§7プレイ可能"
	case apiclient.StateStarting:
		return title + "\n§e起動中…"
	default:
		return title + "\n§7参加すると起動します"
	}
}

// sanitizeTitle はタイトルから書式コードと改行を取り除き、長さを抑える
// (アップロードした人が MOTD の 2 行目や色を乗っ取れないように)。
func sanitizeTitle(s string) string {
	s = strings.NewReplacer("§", "", "\n", " ", "\r", " ").Replace(s)
	r := []rune(s)
	if len(r) > 48 {
		return string(r[:48]) + "…"
	}
	return s
}

// errIsTimeout は ctx の期限切れによる失敗かどうか。
func errIsTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}
