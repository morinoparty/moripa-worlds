// Package config は環境変数から moripa-worlds の設定を読む。
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Config は moripa-worlds 全体の設定。
type Config struct {
	Namespace       string
	WorldHostSuffix string
	ListenMinecraft string
	ListenHTTP      string

	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3Prefix          string
	S3AccessKeyID     string
	S3SecretAccessKey string

	WorldImage         string
	WorldSchematicsPVC string
	WorldOriginY       int
	WorldMemoryRequest resource.Quantity
	WorldMemoryLimit   resource.Quantity

	MaxWorlds      int
	IdleTimeout    time.Duration
	StartupTimeout time.Duration
	SweepInterval  time.Duration
	DeleteExpired  bool
	LogLevel       slog.Level
}

// Load は getenv から設定を読む。未設定の項目は README の既定値になる。
// NAMESPACE / WORLD_IMAGE / AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY は必須。
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	str := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	required := func(key string) string {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
		return v
	}
	integer := func(key string, def, min int) int {
		s := str(key, "")
		if s == "" {
			return def
		}
		v, err := strconv.Atoi(s)
		if err != nil || v < min {
			errs = append(errs, fmt.Errorf("%s: invalid integer %q (min %d)", key, s, min))
			return def
		}
		return v
	}
	duration := func(key string, def time.Duration) time.Duration {
		s := str(key, "")
		if s == "" {
			return def
		}
		v, err := time.ParseDuration(s)
		if err != nil || v <= 0 {
			errs = append(errs, fmt.Errorf("%s: invalid duration %q", key, s))
			return def
		}
		return v
	}
	boolean := func(key string, def bool) bool {
		s := str(key, "")
		if s == "" {
			return def
		}
		v, err := strconv.ParseBool(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid bool %q", key, s))
			return def
		}
		return v
	}
	quantity := func(key, def string) resource.Quantity {
		s := str(key, def)
		q, err := resource.ParseQuantity(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid quantity %q", key, s))
			return resource.MustParse(def)
		}
		return q
	}

	c := Config{
		Namespace:          required("NAMESPACE"),
		WorldHostSuffix:    strings.ToLower(str("WORLD_HOST_SUFFIX", ".world.site1.dev.morino.party")),
		ListenMinecraft:    str("LISTEN_MINECRAFT", ":25565"),
		ListenHTTP:         str("LISTEN_HTTP", ":8080"),
		S3Endpoint:         str("S3_ENDPOINT", "http://rustfs-svc.rustfs.svc.cluster.local:9000"),
		S3Region:           str("S3_REGION", "us-east-1"),
		S3Bucket:           str("S3_BUCKET", "moripa-utils"),
		S3Prefix:           str("S3_PREFIX", "schematics/"),
		S3AccessKeyID:      required("AWS_ACCESS_KEY_ID"),
		S3SecretAccessKey:  required("AWS_SECRET_ACCESS_KEY"),
		WorldImage:         required("WORLD_IMAGE"),
		WorldSchematicsPVC: str("WORLD_SCHEMATICS_PVC", "schematics"),
		WorldOriginY:       integer("WORLD_ORIGIN_Y", 64, -2048),
		WorldMemoryRequest: quantity("WORLD_MEMORY_REQUEST", "384Mi"),
		WorldMemoryLimit:   quantity("WORLD_MEMORY_LIMIT", "768Mi"),
		MaxWorlds:          integer("MAX_WORLDS", 6, 0),
		IdleTimeout:        duration("IDLE_TIMEOUT", 10*time.Minute),
		StartupTimeout:     duration("STARTUP_TIMEOUT", 3*time.Minute),
		SweepInterval:      duration("SWEEP_INTERVAL", time.Hour),
		DeleteExpired:      boolean("DELETE_EXPIRED", true),
	}
	if !strings.HasPrefix(c.WorldHostSuffix, ".") {
		errs = append(errs, fmt.Errorf("WORLD_HOST_SUFFIX must start with '.': %q", c.WorldHostSuffix))
	}
	if !strings.HasSuffix(c.S3Prefix, "/") {
		c.S3Prefix += "/"
	}
	if err := c.LogLevel.UnmarshalText([]byte(str("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	return c, errors.Join(errs...)
}
