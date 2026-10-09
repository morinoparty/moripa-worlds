// moripa-worlds は MoripaUtils がアップロードした schematic を ianua-silvae のワールドとして
// <uuid>.world.site1.dev.morino.party で遊べるようにする。1 つのバイナリに 2 つの役割がある。
//
//	moripa-worlds controller  ワールドの作成 (API)・片付け (reaper / sweeper)。Kubernetes と S3 に触れる
//	moripa-worlds proxy       Gate を組み込んだプロキシ。ロビーで待たせ、起動したワールドへ移す。
//	                          コントローラーの API だけを使い、Kubernetes / S3 の権限は持たない
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/time/rate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/morinoparty/moripa-worlds/internal/api"
	"github.com/morinoparty/moripa-worlds/internal/config"
	"github.com/morinoparty/moripa-worlds/internal/httpserver"
	"github.com/morinoparty/moripa-worlds/internal/mcproto"
	"github.com/morinoparty/moripa-worlds/internal/metrics"
	"github.com/morinoparty/moripa-worlds/internal/proxy"
	"github.com/morinoparty/moripa-worlds/internal/schematic"
	"github.com/morinoparty/moripa-worlds/internal/world"
)

// version はビルド時に -ldflags "-X main.version=..." で埋め込む。
var version = "dev"

// 動作パラメーター (契約で固定のもの)。
const (
	reapInterval        = 30 * time.Second
	creationRatePerMin  = 10
	statusPingTimeout   = 3 * time.Second
	shutdownGracePeriod = 10 * time.Second
)

const usage = `usage: moripa-worlds <command>

commands:
  controller   ワールドの作成 API・reaper・sweeper (Kubernetes / S3 を使う)
  proxy        Gate を組み込んだプロキシ (ロビーで待たせてワールドへ移す)

flags:
  --version    バージョンを表示する

設定はすべて環境変数で渡す (README 参照)。
`

func main() {
	os.Exit(realMain(os.Args[1:]))
}

func realMain(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	var run func() error
	switch args[0] {
	case "-version", "--version", "version":
		fmt.Println(version)
		return 0
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
		return 0
	case "controller":
		run = runController
	case "proxy":
		run = runProxy
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if len(args) > 1 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %q (configuration is read from environment variables)\n", args[1:])
		return 2
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		return 1
	}
	return 0
}

func runProxy() error {
	cfg, err := config.LoadProxy(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	log.Info("starting moripa-worlds proxy", "version", version, "suffix", cfg.WorldHostSuffix,
		"protocol", cfg.RequiredProtocol, "startup_wait", cfg.StartupWait.String())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return proxy.Run(ctx, cfg, log)
}

func runController() error {
	cfg, err := config.LoadController(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	log.Info("starting moripa-worlds controller", "version", version, "namespace", cfg.Namespace, "suffix", cfg.WorldHostSuffix)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	restCfg, err := kubeConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	restCfg.UserAgent = "moripa-worlds/" + version
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	factory := informers.NewSharedInformerFactoryWithOptions(client, 10*time.Minute,
		informers.WithNamespace(cfg.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = world.ManagedSelector }),
	)
	deployInformer := factory.Apps().V1().Deployments()
	lister := deployInformer.Lister()
	synced := deployInformer.Informer().HasSynced

	manager := &world.Manager{
		Client: client,
		Lister: lister,
		Settings: world.Settings{
			Namespace:     cfg.Namespace,
			Image:         cfg.WorldImage,
			SchematicsPVC: cfg.WorldSchematicsPVC,
			OriginY:       cfg.WorldOriginY,
			MemoryRequest: cfg.WorldMemoryRequest,
			MemoryLimit:   cfg.WorldMemoryLimit,

			ForwardingSecretName: cfg.ForwardingSecretName,
		},
		MaxWorlds: cfg.MaxWorlds,
		Limiter:   rate.NewLimiter(rate.Every(time.Minute/creationRatePerMin), creationRatePerMin),
		Log:       log.With("component", "world"),
	}
	m := metrics.New(func() float64 { return float64(manager.Count()) })
	manager.Metrics = m

	store := schematic.NewStore(schematic.NewS3Client(schematic.S3Options{
		Endpoint:        cfg.S3Endpoint,
		Region:          cfg.S3Region,
		AccessKeyID:     cfg.S3AccessKeyID,
		SecretAccessKey: cfg.S3SecretAccessKey,
	}), schematic.StoreOptions{
		Bucket: cfg.S3Bucket,
		Prefix: cfg.S3Prefix,
		Logger: log.With("component", "schematic"),
	})

	factory.Start(ctx.Done())

	apiSrv := &api.Server{
		Token:   cfg.APIToken,
		Store:   store,
		Worlds:  manager,
		Address: func(id string) string { return world.Address(cfg.Namespace, id) },
		Ping: func(ctx context.Context, addr string) error {
			_, err := mcproto.Ping(ctx, addr, statusPingTimeout)
			return err
		},
		Log:     log.With("component", "api"),
		Metrics: m,
	}
	mux := httpserver.New(m.Registry, map[string]httpserver.ReadyCheck{
		"informer": func(context.Context) error {
			if !synced() {
				return errors.New("deployment informer not synced")
			}
			return nil
		},
		"s3": store.Ping,
	})
	apiSrv.Register(mux)
	httpSrv := &http.Server{
		Addr:              cfg.ListenHTTP,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		Handler:           mux,
	}
	var lc net.ListenConfig
	httpLn, err := lc.Listen(ctx, "tcp", cfg.ListenHTTP)
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Go(func() {
		if err := httpSrv.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http: %w", err)
		}
	})

	// informer のキャッシュが揃ってから reaper / sweeper を動かす
	// (API は揃うまで Deployment を見落とすが、ensure は API サーバーで確かめてから作る)。
	if !waitForSync(ctx, synced) {
		return shutdown(httpSrv, &wg, nil)
	}
	log.Info("informer synced", "worlds", manager.Count())

	reaper := &world.Reaper{
		Manager:         manager,
		Store:           store,
		StartupTimeout:  cfg.StartupTimeout,
		IdleTimeout:     cfg.IdleTimeout,
		UnjoinedTimeout: cfg.UnjoinedTimeout,
		Log:             log.With("component", "reaper"),
		Metrics:         m,
		Ping: func(ctx context.Context, addr string) (int, error) {
			r, err := mcproto.Ping(ctx, addr, statusPingTimeout)
			return r.Online, err
		},
	}
	wg.Go(func() { reaper.Run(ctx, reapInterval) })

	if cfg.DeleteExpired {
		sweeper := &schematic.Sweeper{
			Store:   store,
			Log:     log.With("component", "sweeper"),
			Metrics: m,
			DeleteWorld: func(ctx context.Context, id string) error {
				_, getErr := lister.Deployments(cfg.Namespace).Get(world.Name(id))
				if err := manager.Delete(ctx, id); err != nil {
					return err
				}
				if getErr == nil {
					m.Reaped(world.ReasonSwept)
				}
				return nil
			},
		}
		wg.Go(func() { sweeper.Run(ctx, cfg.SweepInterval) })
	}

	log.Info("ready", "http", httpLn.Addr().String())
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		stop()
		return shutdown(httpSrv, &wg, err)
	}
	return shutdown(httpSrv, &wg, nil)
}

func shutdown(srv *http.Server, wg *sync.WaitGroup, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
	defer cancel()
	_ = srv.Shutdown(ctx)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("timed out waiting for goroutines")
	}
	return cause
}

func waitForSync(ctx context.Context, synced func() bool) bool {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for !synced() {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
	return true
}

// kubeConfig はクラスター内ならサービスアカウント、外なら KUBECONFIG を使う (開発用)。
func kubeConfig() (*rest.Config, error) {
	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}
