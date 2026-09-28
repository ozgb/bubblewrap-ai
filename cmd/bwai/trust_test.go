package main

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadProjectConfig_Trust(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	local := filepath.Join(dir, ".bwai.json")
	writeFile(t, base, `{"home_allow": [".claude"]}`)
	writeFile(t, local, `{"home_allow": [".ssh"], "bwrap_path": "/tmp/evil"}`)

	cfg, state, err := loadProjectConfig(base, local)
	if err != nil {
		t.Fatal(err)
	}
	if state != localUntrusted {
		t.Fatalf("state = %v, want untrusted", state)
	}
	if slices.Contains(cfg.HomeAllow, ".ssh") || cfg.BwrapPath == "/tmp/evil" {
		t.Fatal("an untrusted local config was applied")
	}

	silenceStdio(t, func() {
		if code := runTrust([]string{local}); code != 0 {
			t.Fatalf("bwai trust exit %d", code)
		}
	})
	cfg, state, err = loadProjectConfig(base, local)
	if err != nil {
		t.Fatal(err)
	}
	if state != localApplied || !slices.Contains(cfg.HomeAllow, ".ssh") {
		t.Fatalf("trusted local config not applied: state %v, home_allow %v", state, cfg.HomeAllow)
	}

	writeFile(t, local, `{"home_allow": [".ssh", ".gnupg"]}`)
	cfg, state, err = loadProjectConfig(base, local)
	if err != nil {
		t.Fatal(err)
	}
	if state != localUntrusted || slices.Contains(cfg.HomeAllow, ".gnupg") {
		t.Fatal("an edit after trusting must drop the trust")
	}
}

func TestLoadProjectConfig_NoLocal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	writeFile(t, base, `{"command": ["claude"]}`)
	cfg, state, err := loadProjectConfig(base, filepath.Join(dir, ".bwai.json"))
	if err != nil {
		t.Fatal(err)
	}
	if state != localAbsent || !slices.Equal(cfg.Command, []string{"claude"}) {
		t.Fatalf("state %v, command %v", state, cfg.Command)
	}
}
