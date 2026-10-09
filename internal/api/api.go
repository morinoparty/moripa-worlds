// Package api はコントローラー (moripa-worlds controller) の HTTP API を提供する。
// プロキシ (moripa-worlds proxy) はこの API だけを通してワールドの状態を知り、起動を頼む。
// プロキシには Kubernetes の権限も S3 のキーも渡さない。
//
//	GET  /v1/worlds/{id}         → apiclient.World
//	POST /v1/worlds/{id}/ensure  → apiclient.World (result 付き)
//
// /v1/ 以下は "Authorization: Bearer <API_TOKEN>" が必要 (定数時間で比較する)。
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/morinoparty/moripa-worlds/internal/apiclient"
	"github.com/morinoparty/moripa-worlds/internal/metrics"
	"github.com/morinoparty/moripa-worlds/internal/schematic"
	"github.com/morinoparty/moripa-worlds/internal/world"
	"github.com/morinoparty/moripa-worlds/internal/worldid"
)

// InfoGetter は info.json を (キャッシュ経由で) 取得する。
type InfoGetter interface {
	Get(ctx context.Context, id string) (*schematic.Info, error)
}

// Worlds はワールドの状態の参照と作成。*world.Manager が満たす。
type Worlds interface {
	// Status は Deployment があるか (削除中を含む) と、Pod が Ready かを返す。API サーバーは叩かないこと。
	Status(id string) (exists, ready bool)
	// Ensure はワールドを (なければ) 作る。
	Ensure(ctx context.Context, info *schematic.Info) (world.EnsureResult, error)
}

// Pinger はワールドのサーバーにステータス ping を送り、応答があれば nil を返す。
type Pinger func(ctx context.Context, addr string) error

// Server は API のハンドラー。
type Server struct {
	// Token は API_TOKEN。空なら全リクエストを 401 にする。
	Token  string
	Store  InfoGetter
	Worlds Worlds
	// Address は id のワールドの接続先 ("w-<id>.<ns>.svc.cluster.local:25565")。
	Address func(id string) string
	// Ping は ready の判定に使う (任意)。nil なら Deployment が Ready なら ready とする。
	Ping Pinger
	// PingTTL は ping の成功を覚えておく時間 (既定 2 秒)。プロキシ 2 台 × 待っているワールドの
	// 問い合わせが毎秒来ても、ワールドへの ping はこの間隔に 1 回にする。
	PingTTL time.Duration
	Now     func() time.Time
	Log     *slog.Logger
	Metrics *metrics.Metrics

	pingSF    singleflight.Group
	mu        sync.Mutex
	pingOKAt  map[string]time.Time
	tokenHash [32]byte
	hashOnce  sync.Once
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Register は mux に /v1/ のハンドラーを登録する。
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("GET /v1/worlds/{id}", s.auth("state", http.HandlerFunc(s.handleState)))
	mux.Handle("POST /v1/worlds/{id}/ensure", s.auth("ensure", http.HandlerFunc(s.handleEnsure)))
}

// Handler は /v1/ だけを持つハンドラーを返す (テスト・開発用)。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

// auth は Bearer トークンを確かめる。比較は SHA-256 にしてから定数時間で行う
// (長さの違いも漏らさない)。
func (s *Server) auth(op string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hashOnce.Do(func() { s.tokenHash = sha256.Sum256([]byte(s.Token)) })
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		gotHash := sha256.Sum256([]byte(got))
		if !ok || s.Token == "" || subtle.ConstantTimeCompare(gotHash[:], s.tokenHash[:]) != 1 {
			s.Metrics.APIRequest(op, "unauthorized")
			w.Header().Set("WWW-Authenticate", `Bearer realm="moripa-worlds"`)
			writeJSON(w, http.StatusUnauthorized, apiclient.ErrorBody{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// lookup は info.json を読み、World の found / title を埋める。
// 見つからない・不正・期限切れなら found=false (エラーではない)。S3 のエラーは err で返す。
func (s *Server) lookup(ctx context.Context, id string) (*apiclient.World, *schematic.Info, error) {
	w := &apiclient.World{ID: id, State: apiclient.StateAbsent, Address: s.Address(id)}
	info, err := s.Store.Get(ctx, id)
	switch {
	case errors.Is(err, schematic.ErrNotFound):
		return w, nil, nil
	case err != nil:
		return nil, nil, err
	case info.Expired(s.now()):
		return w, nil, nil
	}
	w.Found = true
	if info.Title != nil && *info.Title != "" {
		t := *info.Title
		w.Title = &t
	}
	return w, info, nil
}

// fillState は Deployment と (あれば) ステータス ping から state を決める。
func (s *Server) fillState(ctx context.Context, w *apiclient.World) {
	exists, ready := s.Worlds.Status(w.ID)
	switch {
	case !exists:
		w.State = apiclient.StateAbsent
	case ready && s.pingOK(ctx, w.ID, w.Address):
		w.State = apiclient.StateReady
	default:
		w.State = apiclient.StateStarting
	}
}

// pingOK はワールドがステータス ping に応答するかを返す。成功は PingTTL の間覚えておく。
func (s *Server) pingOK(ctx context.Context, id, addr string) bool {
	if s.Ping == nil {
		return true
	}
	ttl := s.PingTTL
	if ttl <= 0 {
		ttl = 2 * time.Second
	}
	now := s.now()
	s.mu.Lock()
	if t, ok := s.pingOKAt[id]; ok && now.Sub(t) < ttl {
		s.mu.Unlock()
		return true
	}
	s.mu.Unlock()
	ch := s.pingSF.DoChan(id, func() (any, error) {
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		err := s.Ping(pctx, addr)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.pingOKAt == nil {
			s.pingOKAt = map[string]time.Time{}
		}
		if err == nil {
			// 古いものを掃除してから記録する。
			if len(s.pingOKAt) > 1024 {
				for k, t := range s.pingOKAt {
					if s.now().Sub(t) >= ttl {
						delete(s.pingOKAt, k)
					}
				}
			}
			s.pingOKAt[id] = s.now()
		} else {
			delete(s.pingOKAt, id)
		}
		return nil, err
	})
	select {
	case r := <-ch:
		return r.Err == nil
	case <-ctx.Done():
		return false
	}
}

func (s *Server) id(w http.ResponseWriter, r *http.Request, op string) (string, bool) {
	id := r.PathValue("id")
	if !worldid.Valid(id) {
		s.Metrics.APIRequest(op, "bad_request")
		writeJSON(w, http.StatusBadRequest, apiclient.ErrorBody{Error: "id must be a canonical lowercase UUIDv7"})
		return "", false
	}
	return id, true
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	id, ok := s.id(w, r, "state")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	res, _, err := s.lookup(ctx, id)
	if err != nil {
		s.log().Error("failed to look up schematic", "id", id, "error", err)
		s.Metrics.APIRequest("state", "error")
		writeJSON(w, http.StatusServiceUnavailable, apiclient.ErrorBody{Error: "schematic lookup failed"})
		return
	}
	if !res.Found {
		s.Metrics.APIRequest("state", "not_found")
		writeJSON(w, http.StatusOK, res)
		return
	}
	s.fillState(ctx, res)
	s.Metrics.APIRequest("state", string(res.State))
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleEnsure(w http.ResponseWriter, r *http.Request) {
	id, ok := s.id(w, r, "ensure")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, info, err := s.lookup(ctx, id)
	if err != nil {
		s.log().Error("failed to look up schematic", "id", id, "error", err)
		s.Metrics.APIRequest("ensure", "error")
		writeJSON(w, http.StatusOK, &apiclient.World{
			ID: id, State: apiclient.StateAbsent, Address: s.Address(id), Result: apiclient.ResultError,
		})
		return
	}
	if !res.Found {
		res.Result = apiclient.ResultNotFound
		s.Metrics.APIRequest("ensure", string(res.Result))
		writeJSON(w, http.StatusOK, res)
		return
	}
	er, err := s.Worlds.Ensure(ctx, info)
	switch {
	case err != nil:
		s.log().Error("failed to ensure world", "id", id, "error", err)
		res.Result = apiclient.ResultError
	case er == world.Created:
		res.Result = apiclient.ResultCreated
	case er == world.Existing:
		res.Result = apiclient.ResultExisting
	case er == world.Full:
		res.Result = apiclient.ResultFull
	case er == world.RateLimited:
		res.Result = apiclient.ResultRateLimited
	default:
		res.Result = apiclient.ResultError
		s.log().Error("unexpected ensure result", "id", id, "result", fmt.Sprint(er))
	}
	if res.Result == apiclient.ResultCreated {
		// 作ったばかりの Deployment は informer にまだ見えないことがあるので、起動中として返す。
		res.State = apiclient.StateStarting
	} else {
		s.fillState(ctx, res)
	}
	s.Metrics.APIRequest("ensure", string(res.Result))
	if res.Result == apiclient.ResultCreated || res.Result == apiclient.ResultError {
		s.log().Info("ensure", "id", id, "result", res.Result)
	}
	writeJSON(w, http.StatusOK, res)
}
