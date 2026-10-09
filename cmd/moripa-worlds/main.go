// moripa-worlds は MoripaUtils がアップロードした schematic を、
// <uuid>.world.site1.dev.morino.party への接続をきっかけに ianua-silvae のワールドとして起動し、
// 使われなくなったら片付けるコントローラー。
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

	"github.com/morinoparty/moripa-worlds/internal/config"
	"github.com/morinoparty/moripa-worlds/internal/httpserver"
	"github.com/morinoparty/moripa-worlds/internal/mcproto"
	"github.com/morinoparty/moripa-worlds/internal/metrics"
	"github.com/morinoparty/moripa-worlds/internal/schematic"
	"github.com/morinoparty/moripa-worlds/internal/waker"
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

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	log.Info("starting moripa-worlds", "version", version, "namespace", cfg.Namespace, "suffix", cfg.WorldHostSuffix)

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

	mcLn, err := net.Listen("tcp", cfg.ListenMinecraft)
	if err != nil {
		return fmt.Errorf("listen minecraft: %w", err)
	}
	httpSrv := &http.Server{
		Addr:              cfg.ListenHTTP,
		ReadHeaderTimeout: 5 * time.Second,
		Handler: httpserver.New(m.Registry, map[string]httpserver.ReadyCheck{
			"informer": func(context.Context) error {
				if !synced() {
					return errors.New("deployment informer not synced")
				}
				return nil
			},
			"s3": store.Ping,
		}),
	}
	httpLn, err := net.Listen("tcp", cfg.ListenHTTP)
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

	// informer のキャッシュが揃ってから waker / reaper / sweeper を動かす。
	if !waitForSync(ctx, synced) {
		_ = mcLn.Close()
		return shutdown(httpSrv, &wg, nil)
	}
	log.Info("informer synced", "worlds", manager.Count())

	wk := &waker.Server{
		HostSuffix: cfg.WorldHostSuffix,
		Store:      store,
		Worlds:     manager,
		Log:        log.With("component", "waker"),
		Metrics:    m,
	}
	wg.Go(func() {
		if err := wk.Serve(ctx, mcLn); err != nil {
			errCh <- fmt.Errorf("waker: %w", err)
		}
	})

	reaper := &world.Reaper{
		Manager:        manager,
		Store:          store,
		StartupTimeout: cfg.StartupTimeout,
		IdleTimeout:    cfg.IdleTimeout,
		Log:            log.With("component", "reaper"),
		Metrics:        m,
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

	log.Info("ready", "minecraft", mcLn.Addr().String(), "http", httpLn.Addr().String())
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
