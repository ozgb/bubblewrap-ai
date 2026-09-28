package main

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestSessionReloader(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	local := filepath.Join(dir, ".bwai.json")
	writeFile(t, base, `{"broker":{"rules":[{"match":["git-sign"],"action":"auto_allow"}]}}`)
	writeFile(t, local, `{"broker":{"push_allowed_urls":["https://github.com/o/narrow"]}}`)
	silenceStdio(t, func() { runTrust([]string{local}) })

	cfg, state, err := loadProjectConfig(base, local)
	if err != nil || state != localApplied {
		t.Fatalf("setup: state %v, err %v", state, err)
	}
	b := newTestBroker(t, cfg.Broker, dir)
	var rendered []Rule
	reload := sessionReloader(b, base, local, true, func(bc BrokerConfig) { rendered = bc.Rules })

	// A global edit applies, with the trusted local layer still on top.
	writeFile(t, base, `{"broker":{"rules":[{"match":["git-sign"],"action":"auto_allow"},{"match":["gh","status"],"action":"auto_allow"}]}}`)
	if _, err := reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := b.conf(); len(got.Rules) != 2 || !slices.Equal(got.PushAllowedURLs, []string{"https://github.com/o/narrow"}) {
		t.Fatalf("after a global edit: %+v", got)
	}
	if len(rendered) != 2 {
		t.Errorf("the agent's rule list was not re-rendered: %v", rendered)
	}

	// An untrusted edit to an applied local file keeps the previous config,
	// rather than falling back to global-only (which would drop the narrowed
	// push list).
	writeFile(t, local, `{"broker":{"push_allowed_urls":["https://github.com/o/narrow","https://evil.example/x"]}}`)
	if _, err := reload(); err == nil {
		t.Fatal("an untrusted local edit must not reload")
	}
	if got := b.conf().PushAllowedURLs; !slices.Equal(got, []string{"https://github.com/o/narrow"}) {
		t.Fatalf("push list changed to %v after an untrusted edit", got)
	}

	// Trusting the edit lets it through.
	silenceStdio(t, func() { runTrust([]string{local}) })
	if _, err := reload(); err != nil {
		t.Fatalf("reload after trust: %v", err)
	}
	if got := b.conf().PushAllowedURLs; len(got) != 2 {
		t.Fatalf("trusted edit not applied: %v", got)
	}
}

func TestSessionReloader_UntrustedFromTheStart(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	local := filepath.Join(dir, ".bwai.json")
	writeFile(t, base, `{"broker":{"rules":[]}}`)
	writeFile(t, local, `{"broker":{"rules":[{"match":["bash","**"],"action":"auto_allow"}]}}`)

	b := newTestBroker(t, BrokerConfig{}, dir)
	reload := sessionReloader(b, base, local, false, nil)
	writeFile(t, base, `{"broker":{"rules":[{"match":["gh","status"],"action":"auto_allow"}]}}`)
	if _, err := reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	rules := b.conf().Rules
	if len(rules) != 1 || rules[0].Match[0] != "gh" {
		t.Fatalf("rules = %v, want the global edit without the untrusted local rule", rules)
	}
}
