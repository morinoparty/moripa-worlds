package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func base() map[string]string {
	return map[string]string{
		"NAMESPACE":             "worlds",
		"WORLD_IMAGE":           "ghcr.io/morinoparty/ianua-silvae:main",
		"AWS_ACCESS_KEY_ID":     "ak",
		"AWS_SECRET_ACCESS_KEY": "sk",
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.WorldHostSuffix != ".world.site1.dev.morino.party" || c.ListenMinecraft != ":25565" || c.ListenHTTP != ":8080" {
		t.Errorf("listen/suffix defaults: %+v", c)
	}
	if c.S3Endpoint != "http://rustfs-svc.rustfs.svc.cluster.local:9000" || c.S3Region != "us-east-1" || c.S3Bucket != "moripa-utils" || c.S3Prefix != "schematics/" {
		t.Errorf("s3 defaults: %+v", c)
	}
	if c.WorldSchematicsPVC != "schematics" || c.WorldOriginY != 64 || c.WorldMemoryRequest.String() != "384Mi" || c.WorldMemoryLimit.String() != "768Mi" {
		t.Errorf("world defaults: %+v", c)
	}
	if c.MaxWorlds != 6 || c.IdleTimeout != 10*time.Minute || c.StartupTimeout != 3*time.Minute || c.SweepInterval != time.Hour || !c.DeleteExpired || c.LogLevel != slog.LevelInfo {
		t.Errorf("loop defaults: %+v", c)
	}
}

func TestInvalid(t *testing.T) {
	m := base()
	delete(m, "WORLD_IMAGE")
	m["MAX_WORLDS"] = "-1"
	m["IDLE_TIMEOUT"] = "soon"
	m["LOG_LEVEL"] = "loud"
	m["WORLD_HOST_SUFFIX"] = "world.example"
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"WORLD_IMAGE", "MAX_WORLDS", "IDLE_TIMEOUT", "LOG_LEVEL", "WORLD_HOST_SUFFIX"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestOverrides(t *testing.T) {
	m := base()
	m["S3_PREFIX"] = "foo"
	m["WORLD_HOST_SUFFIX"] = ".World.Example"
	m["LOG_LEVEL"] = "debug"
	m["DELETE_EXPIRED"] = "false"
	c, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.S3Prefix != "foo/" || c.WorldHostSuffix != ".world.example" || c.LogLevel != slog.LevelDebug || c.DeleteExpired {
		t.Errorf("%+v", c)
	}
}
