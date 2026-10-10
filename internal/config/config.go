// Package config は環境変数から moripa-worlds の設定を読む。
// サブコマンドごとに LoadController (controller) と LoadProxy (proxy) がある。
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Controller は controller サブコマンドの設定。
type Controller struct {
	Namespace       string
	WorldHostSuffix string
	ListenHTTP      string
	// APIToken はプロキシが API を呼ぶときの Bearer トークン。
	APIToken string

	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3Prefix          string
	S3AccessKeyID     string
	S3SecretAccessKey string

	WorldImage         string
	WorldSchematicsPVC string
	WorldOriginY       int
	WorldCPURequest    resource.Quantity
	WorldMemoryRequest resource.Quantity
	WorldMemoryLimit   resource.Quantity
	// ForwardingSecretName はワールドの Pod に IANUA_VELOCITY_SECRET として渡す Secret の名前 (キー "secret")。
	ForwardingSecretName string

	MaxWorlds       int
	IdleTimeout     time.Duration
	UnjoinedTimeout time.Duration
	StartupTimeout  time.Duration
	SweepInterval   time.Duration
	DeleteExpired   bool
	LogLevel        slog.Level
}

// Proxy は proxy サブコマンド (Gate を組み込んだプロキシ) の設定。
type Proxy struct {
	// GateConfig は Gate の設定ファイル (config.yml) のパス。
	GateConfig string
	// ForwardingSecret は Velocity modern forwarding の秘密。設定ファイルには書かず環境変数で渡す。
	ForwardingSecret string
	// APIURL はコントローラーの API の URL (末尾 "/" なし)。
	APIURL   string
	APIToken string

	WorldHostSuffix string
	// LobbyServer はワールドの起動を待つ間にプレイヤーを置くサーバーの名前 (Gate の servers のキー)。
	LobbyServer string
	// RequiredProtocol / RequiredVersionName はワールド (ianua-silvae) が受け付ける唯一のバージョン。
	RequiredProtocol    int
	RequiredVersionName string
	// StartupWait はワールドの起動を待つ上限。コントローラーの STARTUP_TIMEOUT に揃える。
	StartupWait time.Duration
	ListenHTTP  string
	LogLevel    slog.Level
}

// loader は環境変数を読み、エラーをまとめる。
type loader struct {
	getenv func(string) string
	errs   []error
}

func (l *loader) str(key, def string) string {
	if v := strings.TrimSpace(l.getenv(key)); v != "" {
		return v
	}
	return def
}

func (l *loader) required(key string) string {
	v := strings.TrimSpace(l.getenv(key))
	if v == "" {
		l.errs = append(l.errs, fmt.Errorf("%s is required", key))
	}
	return v
}

// secret は必須の秘密を読む。値はエラーメッセージに出さない。
func (l *loader) secret(key string, minLen int) string {
	v := l.required(key)
	if v != "" && len(v) < minLen {
		l.errs = append(l.errs, fmt.Errorf("%s must be at least %d characters", key, minLen))
	}
	return v
}

func (l *loader) integer(key string, def, minimum int) int {
	s := l.str(key, "")
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < minimum {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid integer %q (min %d)", key, s, minimum))
		return def
	}
	return v
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	s := l.str(key, "")
	if s == "" {
		return def
	}
	v, err := time.ParseDuration(s)
	if err != nil || v <= 0 {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid duration %q", key, s))
		return def
	}
	return v
}

func (l *loader) boolean(key string, def bool) bool {
	s := l.str(key, "")
	if s == "" {
		return def
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid bool %q", key, s))
		return def
	}
	return v
}

func (l *loader) quantity(key, def string) resource.Quantity {
	s := l.str(key, def)
	q, err := resource.ParseQuantity(s)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid quantity %q", key, s))
		return resource.MustParse(def)
	}
	return q
}

func (l *loader) suffix() string {
	s := strings.ToLower(l.str("WORLD_HOST_SUFFIX", ".world.site1.dev.morino.party"))
	if !strings.HasPrefix(s, ".") {
		l.errs = append(l.errs, fmt.Errorf("WORLD_HOST_SUFFIX must start with '.': %q", s))
	}
	return s
}

func (l *loader) logLevel() slog.Level {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(l.str("LOG_LEVEL", "info"))); err != nil {
		l.errs = append(l.errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	return lv
}

// MinTokenLen は API_TOKEN / FORWARDING_SECRET の最低の長さ。
const MinTokenLen = 16

// LoadController は getenv から controller の設定を読む。未設定の項目は README の既定値になる。
// NAMESPACE / WORLD_IMAGE / AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / API_TOKEN は必須。
func LoadController(getenv func(string) string) (Controller, error) {
	l := &loader{getenv: getenv}
	c := Controller{
		Namespace:            l.required("NAMESPACE"),
		WorldHostSuffix:      l.suffix(),
		ListenHTTP:           l.str("LISTEN_HTTP", ":8080"),
		APIToken:             l.secret("API_TOKEN", MinTokenLen),
		S3Endpoint:           l.str("S3_ENDPOINT", "http://rustfs-svc.rustfs.svc.cluster.local:9000"),
		S3Region:             l.str("S3_REGION", "us-east-1"),
		S3Bucket:             l.str("S3_BUCKET", "moripa-utils"),
		S3Prefix:             l.str("S3_PREFIX", "schematics/"),
		S3AccessKeyID:        l.required("AWS_ACCESS_KEY_ID"),
		S3SecretAccessKey:    l.required("AWS_SECRET_ACCESS_KEY"),
		WorldImage:           l.required("WORLD_IMAGE"),
		WorldSchematicsPVC:   l.str("WORLD_SCHEMATICS_PVC", "schematics"),
		WorldOriginY:         l.integer("WORLD_ORIGIN_Y", 64, -2048),
		WorldCPURequest:      l.quantity("WORLD_CPU_REQUEST", "200m"),
		WorldMemoryRequest:   l.quantity("WORLD_MEMORY_REQUEST", "384Mi"),
		WorldMemoryLimit:     l.quantity("WORLD_MEMORY_LIMIT", "768Mi"),
		ForwardingSecretName: l.str("FORWARDING_SECRET_NAME", "worlds-forwarding"),
		MaxWorlds:            l.integer("MAX_WORLDS", 6, 0),
		IdleTimeout:          l.duration("IDLE_TIMEOUT", 10*time.Minute),
		UnjoinedTimeout:      l.duration("UNJOINED_TIMEOUT", 3*time.Minute),
		StartupTimeout:       l.duration("STARTUP_TIMEOUT", 5*time.Minute),
		SweepInterval:        l.duration("SWEEP_INTERVAL", time.Hour),
		DeleteExpired:        l.boolean("DELETE_EXPIRED", true),
		LogLevel:             l.logLevel(),
	}
	if !strings.HasSuffix(c.S3Prefix, "/") {
		c.S3Prefix += "/"
	}
	return c, errors.Join(l.errs...)
}

// LoadProxy は getenv から proxy の設定を読む。API_TOKEN / FORWARDING_SECRET は必須。
func LoadProxy(getenv func(string) string) (Proxy, error) {
	l := &loader{getenv: getenv}
	c := Proxy{
		GateConfig:          l.str("GATE_CONFIG", "/etc/gate/config.yml"),
		ForwardingSecret:    l.secret("FORWARDING_SECRET", MinTokenLen),
		APIURL:              strings.TrimRight(l.str("API_URL", "http://moripa-worlds.worlds.svc.cluster.local:8080"), "/"),
		APIToken:            l.secret("API_TOKEN", MinTokenLen),
		WorldHostSuffix:     l.suffix(),
		LobbyServer:         l.str("LOBBY_SERVER", "lobby"),
		RequiredProtocol:    l.integer("REQUIRED_PROTOCOL", 777, 1),
		RequiredVersionName: l.str("REQUIRED_VERSION_NAME", "26.3"),
		StartupWait:         l.duration("STARTUP_WAIT", 5*time.Minute),
		ListenHTTP:          l.str("LISTEN_HTTP", ":8080"),
		LogLevel:            l.logLevel(),
	}
	if u, err := url.Parse(c.APIURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		l.errs = append(l.errs, fmt.Errorf("API_URL: invalid URL %q", c.APIURL))
	}
	return c, errors.Join(l.errs...)
}
