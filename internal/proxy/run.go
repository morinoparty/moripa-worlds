package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.minekube.com/gate/pkg/edition/java/config"
	jproxy "go.minekube.com/gate/pkg/edition/java/proxy"
	"go.minekube.com/gate/pkg/gate"
	gconfig "go.minekube.com/gate/pkg/gate/config"

	"github.com/morinoparty/moripa-worlds/internal/apiclient"
	mconfig "github.com/morinoparty/moripa-worlds/internal/config"
	"github.com/morinoparty/moripa-worlds/internal/httpserver"
)

// LoadGateConfig は Gate の設定ファイルを読み、Velocity の秘密を差し込んで、
// この構成で守るべき設定を確かめる。秘密は設定ファイル (ConfigMap) には置かない。
func LoadGateConfig(path, forwardingSecret, lobby string) (*gconfig.Config, error) {
	v := gate.NewViper()
	v.SetConfigFile(path)
	cfg, err := gate.LoadConfig(v)
	if err != nil {
		return nil, fmt.Errorf("gate config %s: %w", path, err)
	}
	if err := PrepareGateConfig(cfg, forwardingSecret, lobby); err != nil {
		return nil, fmt.Errorf("gate config %s: %w", path, err)
	}
	return cfg, nil
}

// PrepareGateConfig は読み込んだ Gate の設定に秘密を入れ、前提を確かめる。
func PrepareGateConfig(cfg *gconfig.Config, forwardingSecret, lobby string) error {
	c := &cfg.Config
	var errs []error
	if c.Lite.Enabled {
		errs = append(errs, errors.New("lite.enabled must be false (classic mode is required)"))
	}
	if c.Forwarding.Mode != config.VelocityForwardingMode {
		errs = append(errs, fmt.Errorf("forwarding.mode must be %q, got %q", config.VelocityForwardingMode, c.Forwarding.Mode))
	}
	if c.Forwarding.VelocitySecret != "" {
		errs = append(errs, errors.New("forwarding.velocitySecret must not be set in the config file; use FORWARDING_SECRET"))
	}
	if forwardingSecret == "" {
		errs = append(errs, errors.New("FORWARDING_SECRET is empty"))
	}
	if _, ok := c.Servers[lobby]; !ok {
		errs = append(errs, fmt.Errorf("servers must contain the lobby %q", lobby))
	}
	if len(c.Try) != 0 || len(c.ForcedHosts) != 0 {
		errs = append(errs, errors.New("try and forcedHosts must be empty (the plugin chooses servers)"))
	}
	if cfg.API.Enabled || cfg.Connect.Enabled {
		errs = append(errs, errors.New("api and connect must be disabled"))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	c.Forwarding.VelocitySecret = forwardingSecret
	// /server などの組み込みコマンドで他のワールドやロビーへ移れないようにする。
	c.BuiltinCommands = false
	return nil
}

// Run は Gate とプラグイン、/healthz・/metrics を動かす。ctx が終わると止まる。
func Run(ctx context.Context, cfg mconfig.Proxy, log *slog.Logger) error {
	gcfg, err := LoadGateConfig(cfg.GateConfig, cfg.ForwardingSecret, cfg.LobbyServer)
	if err != nil {
		return err
	}
	pl := &Plugin{
		Gatekeeper: Gatekeeper{
			Suffix:              cfg.WorldHostSuffix,
			RequiredProtocol:    cfg.RequiredProtocol,
			RequiredVersionName: cfg.RequiredVersionName,
		},
		API:         apiclient.New(cfg.APIURL, cfg.APIToken),
		Lobby:       cfg.LobbyServer,
		StartupWait: cfg.StartupWait,
		Log:         log.With("component", "plugin"),
	}
	pl.Metrics = NewMetrics(pl.Waiting)
	jproxy.Plugins = append(jproxy.Plugins, pl.GatePlugin())

	var lc net.ListenConfig
	httpLn, err := lc.Listen(ctx, "tcp", cfg.ListenHTTP)
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}
	httpSrv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler:           httpserver.New(pl.Metrics.Registry, nil),
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := httpSrv.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "error", err)
		}
	})
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sctx)
		wg.Wait()
	}()

	log.Info("starting gate", "bind", gcfg.Config.Bind, "lobby", cfg.LobbyServer, "api", cfg.APIURL, "http", httpLn.Addr().String())
	gctx := logr.NewContext(ctx, logr.FromSlogHandler(log.Handler()))
	return gate.Start(gctx, gate.WithConfig(*gcfg), gate.WithAutoShutdownOnSignal(false))
}
