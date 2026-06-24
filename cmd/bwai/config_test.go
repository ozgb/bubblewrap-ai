package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebApproveGate(t *testing.T) {
	cases := []struct {
		name   string
		prompt []string
		want   bool
	}{
		{"web present", []string{"web"}, true},
		{"web among others", []string{"oob", "web"}, true},
		{"oob only", []string{"oob"}, false},
		{"empty", []string{}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (BrokerConfig{Prompt: tc.prompt}).webApprove(); got != tc.want {
				t.Errorf("webApprove(%v) = %v, want %v", tc.prompt, got, tc.want)
			}
		})
	}
}

func TestValidateWebAddr(t *testing.T) {
	ok := []string{"127.0.0.1:0", "127.0.0.1:8080", "[::1]:0", "localhost:0"}
	for _, a := range ok {
		if err := validateWebAddr(a); err != nil {
			t.Errorf("validateWebAddr(%q) = %v, want nil", a, err)
		}
	}
	bad := []string{"0.0.0.0:8080", "192.168.1.5:80", ":8080", "8.8.8.8:53", "garbage"}
	for _, a := range bad {
		if err := validateWebAddr(a); err == nil {
			t.Errorf("validateWebAddr(%q) = nil, want error", a)
		}
	}
}

// TestLoadConfigRejectsNonLoopbackWebAddr pins the defence-in-depth gate:
// enabling web mode with a routable bind address must fail to load.
func TestLoadConfigRejectsNonLoopbackWebAddr(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bwai.json")
	cfgJSON := `{"broker":{"enabled":true,"prompt":["web"],"web":{"addr":"0.0.0.0:9000"}}}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("loadConfig accepted a non-loopback web.addr")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error = %v, want it to mention loopback", err)
	}
}

// TestLoadConfigWebDefaults verifies web mode without an explicit addr
// inherits the ephemeral-loopback default rather than binding the wildcard.
func TestLoadConfigWebDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bwai.json")
	cfgJSON := `{"broker":{"enabled":true,"prompt":["web"]}}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Broker.Web.Addr != defaultWebAddr {
		t.Errorf("web.addr = %q, want default %q", cfg.Broker.Web.Addr, defaultWebAddr)
	}
}
