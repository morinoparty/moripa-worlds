// devcontroller は Kubernetes と S3 なしでコントローラーの API (internal/api の本物のハンドラー) を
// 動かす開発用のコマンド。ワールドはメモリ上にだけあり、ensure されてから -ready-after 経つと
// (かつ -address がステータス ping に応答すると) ready になる。プロキシとの結合確認 (hack/e2e) に使う。
//
//	go run ./hack/devcontroller -listen :8080 -address world:25565 \
//	    -world 0199c8a0-7b3e-7f12-9a4c-1d2e3f405162=テストワールド
//
// トークンは環境変数 API_TOKEN で渡す。
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/morinoparty/moripa-worlds/internal/api"
	"github.com/morinoparty/moripa-worlds/internal/mcproto"
	"github.com/morinoparty/moripa-worlds/internal/schematic"
	"github.com/morinoparty/moripa-worlds/internal/world"
)

type memStore map[string]*schematic.Info

func (m memStore) Get(_ context.Context, id string) (*schematic.Info, error) {
	if i, ok := m[id]; ok {
		return i, nil
	}
	return nil, schematic.ErrNotFound
}

// memWorlds は ensure された時刻を覚え、readyAfter 経ったら Ready とする。
type memWorlds struct {
	mu         sync.Mutex
	created    map[string]time.Time
	readyAfter time.Duration
	max        int
	log        *slog.Logger
}

func (w *memWorlds) Status(id string) (exists, ready bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.created[id]
	return ok, ok && time.Since(t) >= w.readyAfter
}

func (w *memWorlds) Ensure(_ context.Context, info *schematic.Info) (world.EnsureResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.created[info.ID]; ok {
		w.log.Info("ensure: existing", "id", info.ID)
		return world.Existing, nil
	}
	if len(w.created) >= w.max {
		w.log.Info("ensure: full", "id", info.ID)
		return world.Full, nil
	}
	w.created[info.ID] = time.Now()
	w.log.Info("ensure: created", "id", info.ID, "ready_after", w.readyAfter.String())
	return world.Created, nil
}

func main() {
	listen := flag.String("listen", ":8080", "listen address")
	address := flag.String("address", "world:25565", "address returned for every world (and pinged for readiness)")
	worlds := flag.String("world", "", "comma separated id=title of known schematics")
	readyAfter := flag.Duration("ready-after", 10*time.Second, "time from ensure until the world counts as ready")
	maxWorlds := flag.Int("max", 6, "MAX_WORLDS")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	token := os.Getenv("API_TOKEN")
	if token == "" {
		log.Error("API_TOKEN is required")
		os.Exit(1)
	}
	store := memStore{}
	for kv := range strings.SplitSeq(*worlds, ",") {
		id, title, _ := strings.Cut(strings.TrimSpace(kv), "=")
		if id == "" {
			continue
		}
		t := title
		store[id] = &schematic.Info{ID: id, Format: schematic.FormatSpongeV3, FileName: "schematic.schem", Title: &t}
	}
	srv := &api.Server{
		Token:   token,
		Store:   store,
		Worlds:  &memWorlds{created: map[string]time.Time{}, readyAfter: *readyAfter, max: *maxWorlds, log: log},
		Address: func(string) string { return *address },
		Ping: func(ctx context.Context, addr string) error {
			_, err := mcproto.Ping(ctx, addr, 2*time.Second)
			return err
		},
		Log: log,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	srv.Register(mux)
	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Info("request", "method", r.Method, "path", r.URL.Path)
		mux.ServeHTTP(w, r)
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", *listen)
	if err != nil {
		log.Error("listen", "error", err)
		return
	}
	hs := &http.Server{Handler: logged, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = hs.Close()
	}()
	log.Info("devcontroller listening", "addr", ln.Addr().String(), "worlds", len(store), "address", *address)
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "error", err)
	}
}
