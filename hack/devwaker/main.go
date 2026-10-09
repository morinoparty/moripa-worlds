// devwaker は Kubernetes と S3 なしで waker だけを動かす開発用のコマンド。
// Gate Lite と組み合わせた動作確認 (README の「Gate との結合確認」) に使う。
//
//	go run ./hack/devwaker -listen 127.0.0.1:35566 -known 0199c8a0-7b3e-7f12-9a4c-1d2e3f405162
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/morinoparty/moripa-worlds/internal/schematic"
	"github.com/morinoparty/moripa-worlds/internal/waker"
	"github.com/morinoparty/moripa-worlds/internal/world"
)

type memStore map[string]*schematic.Info

func (m memStore) Get(_ context.Context, id string) (*schematic.Info, error) {
	if i, ok := m[id]; ok {
		return i, nil
	}
	return nil, schematic.ErrNotFound
}

// logWorlds は最初の Ensure で Created、以降は Existing を返し、呼ばれたことをログに出す。
// Exists は一度 Ensure された ID で true を返す。
type logWorlds struct {
	mu      sync.Mutex
	created map[string]bool
	log     *slog.Logger
}

func (l *logWorlds) Ensure(_ context.Context, info *schematic.Info) (world.EnsureResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.created[info.ID] {
		l.log.Info("ensure: existing", "id", info.ID)
		return world.Existing, nil
	}
	l.created[info.ID] = true
	l.log.Info("ensure: created", "id", info.ID)
	return world.Created, nil
}

func (l *logWorlds) Exists(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.created[id]
}

func main() {
	listen := flag.String("listen", "127.0.0.1:35566", "listen address")
	suffix := flag.String("suffix", ".world.site1.dev.morino.party", "world host suffix")
	known := flag.String("known", "", "comma separated known schematic ids")
	title := flag.String("title", "テストワールド", "title of known schematics")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	store := memStore{}
	for id := range strings.SplitSeq(*known, ",") {
		if id = strings.TrimSpace(id); id != "" {
			store[id] = &schematic.Info{ID: id, Format: schematic.FormatSpongeV3, FileName: "schematic.schem", Title: title}
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", *listen)
	if err != nil {
		log.Error("listen", "error", err)
		return
	}
	log.Info("devwaker listening", "addr", ln.Addr().String(), "known", *known)
	s := &waker.Server{
		HostSuffix: *suffix,
		Store:      store,
		Worlds:     &logWorlds{created: map[string]bool{}, log: log},
		Log:        log,
	}
	if err := s.Serve(ctx, ln); err != nil {
		log.Error("serve", "error", err)
	}
}
