package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallAgentMemoryFile(t *testing.T) {
	tmpDir := t.TempDir()
	rules := []Rule{
		{Match: []string{"git-safe", "push"}, Action: ActionConfirm},
		{Match: []string{"gh", "pr", "create", "**"}, Action: ActionConfirm},
	}
	if err := installAgentMemoryFile(tmpDir, rules); err != nil {
		t.Fatalf("installAgentMemoryFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmpDir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.HasPrefix(s, agentMemoryFileContent) {
		t.Errorf("CLAUDE.md should open with the bwai fragment:\n%s", s)
	}
	// The rules ride along in the fragment so the agent knows them before
	// its first turn, without having to run `bwai-outside --list-rules`.
	for _, want := range []string{"Broker rules for this sandbox", "CONFIRM", "git-safe push", "gh pr create **"} {
		if !strings.Contains(s, want) {
			t.Errorf("CLAUDE.md missing %q:\n%s", want, s)
		}
	}
}

func TestInstallAgentMemoryFileNoRules(t *testing.T) {
	tmpDir := t.TempDir()
	if err := installAgentMemoryFile(tmpDir, nil); err != nil {
		t.Fatalf("installAgentMemoryFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmpDir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	// An empty ruleset means everything is denied; the fragment has to say
	// so rather than showing an empty table.
	if !strings.Contains(string(got), "no rules configured") {
		t.Errorf("deny-all should be stated explicitly:\n%s", got)
	}
}

func TestInstallBwaiMod(t *testing.T) {
	tmpDir := t.TempDir()
	if err := installBwaiMod(tmpDir); err != nil {
		t.Fatalf("installBwaiMod: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmpDir, "bwai.ts"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	// The mod is loaded with `cmd --mod`: it must register the
	// system-prompt hook and read the fragment bwai mounts at
	// /run/bwai/CLAUDE.md.
	for _, want := range []string{"appendSystemPrompt", "/run/bwai/CLAUDE.md", "@commandcode/harness"} {
		if !strings.Contains(s, want) {
			t.Errorf("bwai.ts missing %q:\n%s", want, s)
		}
	}
}
