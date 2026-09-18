package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallAgentMemoryFile(t *testing.T) {
	tmpDir := t.TempDir()
	if err := installAgentMemoryFile(tmpDir); err != nil {
		t.Fatalf("installAgentMemoryFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmpDir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != agentMemoryFileContent {
		t.Errorf("CLAUDE.md = %q, want the bwai fragment", got)
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
