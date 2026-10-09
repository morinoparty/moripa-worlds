package proxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.minekube.com/common/minecraft/component"
	jconfig "go.minekube.com/gate/pkg/edition/java/config"
)

const goodConfig = `config:
  bind: 0.0.0.0:25565
  onlineMode: true
  proxyProtocol: true
  forwarding:
    mode: velocity
  servers:
    lobby: lobby.worlds.svc.cluster.local:25565
  try: []
  lite:
    enabled: false
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadGateConfig(t *testing.T) {
	const secret = "fw-secret-0123456789abcdef"
	cfg, err := LoadGateConfig(writeConfig(t, goodConfig), secret, "lobby")
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Config
	if c.Forwarding.Mode != jconfig.VelocityForwardingMode || c.Forwarding.VelocitySecret != secret {
		t.Fatalf("forwarding = %+v", c.Forwarding)
	}
	if !c.OnlineMode || !c.ProxyProtocol || c.Lite.Enabled || c.BuiltinCommands || len(c.Try) != 0 {
		t.Fatalf("config = %+v", c)
	}
	if c.Servers["lobby"] != "lobby.worlds.svc.cluster.local:25565" {
		t.Fatalf("servers = %v", c.Servers)
	}
	if _, errs := cfg.Validate(); len(errs) != 0 {
		t.Fatalf("validate: %v", errs)
	}
}

func TestLoadGateConfigRejects(t *testing.T) {
	tests := []struct {
		name, body, secret, want string
	}{
		{"lite", strings.Replace(goodConfig, "enabled: false", "enabled: true", 1), "s", "lite.enabled"},
		{"legacy forwarding", strings.Replace(goodConfig, "mode: velocity", "mode: legacy", 1), "s", "forwarding.mode"},
		{"secret in file", strings.Replace(goodConfig, "mode: velocity", "mode: velocity\n    velocitySecret: oops", 1), "s", "velocitySecret"},
		{"no secret", goodConfig, "", "FORWARDING_SECRET"},
		{"no lobby", strings.Replace(goodConfig, "lobby:", "hub:", 1), "s", `lobby "lobby"`},
		{"try", strings.Replace(goodConfig, "try: []", "try: [lobby]", 1), "s", "try"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadGateConfig(writeConfig(t, tt.body), tt.secret, "lobby")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "oops") {
				t.Fatalf("error leaks the secret: %v", err)
			}
		})
	}
	if _, err := LoadGateConfig(filepath.Join(t.TempDir(), "missing.yml"), "s", "lobby"); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestPlainText(t *testing.T) {
	c := &component.Text{Content: "You are ", Extra: []component.Component{
		&component.Translation{Key: "multiplayer.disconnect.banned", With: []component.Component{&component.Text{Content: "griefing"}}},
	}}
	got := plainText(c)
	if !strings.Contains(got, "You are") || !strings.Contains(got, "multiplayer.disconnect.banned") || !strings.Contains(got, "griefing") {
		t.Fatalf("plainText = %q", got)
	}
	if !DeliberateKick(got) {
		t.Fatal("banned translation not detected")
	}
	if plainText(nil) != "" {
		t.Fatal("nil")
	}
}
