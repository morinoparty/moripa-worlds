// Package apiclient はコントローラー (moripa-worlds controller) の HTTP API の型と、
// プロキシ (moripa-worlds proxy) が使うクライアントを提供する。
//
// API:
//
//	GET  /v1/worlds/{id}         ワールドの状態
//	POST /v1/worlds/{id}/ensure  ワールドを (なければ) 起動する
//
// どちらも "Authorization: Bearer <API_TOKEN>" が必要。
package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/morinoparty/moripa-worlds/internal/worldid"
)

// State はワールドの起動状態。
type State string

const (
	// StateAbsent はワールド (Deployment) が無い。参加すると起動する。
	StateAbsent State = "absent"
	// StateStarting はワールドを起動中 (Deployment はあるが、まだ接続できない)。
	StateStarting State = "starting"
	// StateReady はワールドに接続できる。
	StateReady State = "ready"
)

// Result は ensure の結果。
type Result string

const (
	ResultCreated     Result = "created"
	ResultExisting    Result = "existing"
	ResultFull        Result = "full"
	ResultRateLimited Result = "rate_limited"
	ResultNotFound    Result = "not_found"
	ResultError       Result = "error"
)

// World は GET /v1/worlds/{id} と POST /v1/worlds/{id}/ensure の応答。
type World struct {
	ID string `json:"id"`
	// Found は schematic (info.json) があり、期限切れでないかどうか。
	Found bool `json:"found"`
	// Title はワールドのタイトル。Found=false か未設定なら nil。
	Title   *string `json:"title"`
	State   State   `json:"state"`
	Address string  `json:"address"`
	// Result は ensure のときだけ入る。
	Result Result `json:"result,omitempty"`
}

// DisplayTitle はタイトル、なければ既定の名前を返す。
func (w *World) DisplayTitle() string {
	if w.Title != nil && *w.Title != "" {
		return *w.Title
	}
	return "morino.party world"
}

// ErrorBody はエラー応答の本文。
type ErrorBody struct {
	Error string `json:"error"`
}

// MaxResponseSize は応答として読む最大バイト数。
const MaxResponseSize = 64 * 1024

// ErrStatus は API が 2xx 以外を返したことを表す。
type ErrStatus struct {
	Code    int
	Message string
}

func (e *ErrStatus) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("apiclient: status %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("apiclient: status %d", e.Code)
}

// Client はコントローラーの API クライアント。
type Client struct {
	// BaseURL は "http://moripa-worlds.worlds.svc.cluster.local:8080" のような URL (末尾 "/" なし)。
	BaseURL string
	// Token は API_TOKEN。ログに出さないこと。
	Token string
	// HTTP は使う http.Client。nil なら既定 (タイムアウトは各呼び出しの ctx で掛ける)。
	HTTP *http.Client
	// Timeout は 1 回の呼び出しの上限 (既定 3 秒)。ctx の期限が先ならそちら。
	Timeout time.Duration
}

// New は Client を作る。
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP: &http.Client{Transport: &http.Transport{
			Proxy:               nil,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		}},
		Timeout: 3 * time.Second,
	}
}

// State はワールドの状態を返す。
func (c *Client) State(ctx context.Context, id string) (*World, error) {
	return c.do(ctx, http.MethodGet, id, "")
}

// Ensure はワールドを (なければ) 起動する。
func (c *Client) Ensure(ctx context.Context, id string) (*World, error) {
	return c.do(ctx, http.MethodPost, id, "/ensure")
}

func (c *Client) do(ctx context.Context, method, id, suffix string) (*World, error) {
	if !worldid.Valid(id) {
		return nil, worldid.ErrInvalidID
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/v1/worlds/"+id+suffix, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("apiclient: %s %s: %w", method, id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("apiclient: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var eb ErrorBody
		_ = json.Unmarshal(body, &eb)
		return nil, &ErrStatus{Code: resp.StatusCode, Message: eb.Error}
	}
	var w World
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("apiclient: decode response: %w", err)
	}
	if w.ID != id {
		return nil, errors.New("apiclient: response id does not match")
	}
	return &w, nil
}
