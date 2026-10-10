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

const testToken = "0123456789abcdef0123"

func base() map[string]string {
	return map[string]string{
		"NAMESPACE":             "worlds",
		"WORLD_IMAGE":           "ghcr.io/morinoparty/ianua-silvae:main",
		"AWS_ACCESS_KEY_ID":     "ak",
		"AWS_SECRET_ACCESS_KEY": "sk",
		"API_TOKEN":             testToken,
	}
}

func TestDefaults(t *testing.T) {
	c, err := LoadController(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.WorldHostSuffix != ".world.site1.dev.morino.party" || c.ListenHTTP != ":8080" || c.APIToken != testToken {
		t.Errorf("listen/suffix defaults: %+v", c)
	}
	if c.S3Endpoint != "http://rustfs-svc.rustfs.svc.cluster.local:9000" || c.S3Region != "us-east-1" || c.S3Bucket != "moripa-utils" || c.S3Prefix != "schematics/" {
		t.Errorf("s3 defaults: %+v", c)
	}
	if c.WorldSchematicsPVC != "schematics" || c.WorldOriginY != 64 || c.WorldCPURequest.String() != "200m" || c.WorldMemoryRequest.String() != "384Mi" || c.WorldMemoryLimit.String() != "768Mi" || c.ForwardingSecretName != "worlds-forwarding" {
		t.Errorf("world defaults: %+v", c)
	}
	if c.MaxWorlds != 6 || c.IdleTimeout != 10*time.Minute || c.UnjoinedTimeout != 3*time.Minute || c.StartupTimeout != 5*time.Minute || c.SweepInterval != time.Hour || !c.DeleteExpired || c.LogLevel != slog.LevelInfo {
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
	m["API_TOKEN"] = "short-secret"
	_, err := LoadController(env(m))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"WORLD_IMAGE", "MAX_WORLDS", "IDLE_TIMEOUT", "LOG_LEVEL", "WORLD_HOST_SUFFIX", "API_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "short-secret") {
		t.Errorf("error leaks the token: %q", err)
	}
	m = base()
	delete(m, "API_TOKEN")
	if _, err := LoadController(env(m)); err == nil || !strings.Contains(err.Error(), "API_TOKEN is required") {
		t.Errorf("missing API_TOKEN: %v", err)
	}
}

func TestOverrides(t *testing.T) {
	m := base()
	m["S3_PREFIX"] = "foo"
	m["WORLD_HOST_SUFFIX"] = ".World.Example"
	m["LOG_LEVEL"] = "debug"
	m["DELETE_EXPIRED"] = "false"
	m["FORWARDING_SECRET_NAME"] = "fw"
	c, err := LoadController(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.S3Prefix != "foo/" || c.WorldHostSuffix != ".world.example" || c.LogLevel != slog.LevelDebug || c.DeleteExpired || c.ForwardingSecretName != "fw" {
		t.Errorf("%+v", c)
	}
}

func TestProxyDefaults(t *testing.T) {
	c, err := LoadProxy(env(map[string]string{"API_TOKEN": testToken, "FORWARDING_SECRET": "fwsecret-0123456789"}))
	if err != nil {
		t.Fatal(err)
	}
	want := Proxy{
		GateConfig:          "/etc/gate/config.yml",
		ForwardingSecret:    "fwsecret-0123456789",
		APIURL:              "http://moripa-worlds.worlds.svc.cluster.local:8080",
		APIToken:            testToken,
		WorldHostSuffix:     ".world.site1.dev.morino.party",
		LobbyServer:         "lobby",
		RequiredProtocol:    777,
		RequiredVersionName: "26.3",
		StartupWait:         5 * time.Minute,
		ListenHTTP:          ":8080",
		LogLevel:            slog.LevelInfo,
	}
	if c != want {
		t.Errorf("got %+v", c)
	}
}

func TestProxyInvalid(t *testing.T) {
	_, err := LoadProxy(env(map[string]string{"API_URL": "ftp://x", "REQUIRED_PROTOCOL": "abc", "FORWARDING_SECRET": "short"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"API_TOKEN is required", "FORWARDING_SECRET must be at least", "API_URL", "REQUIRED_PROTOCOL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "short") {
		t.Errorf("error leaks the secret: %q", err)
	}
}
