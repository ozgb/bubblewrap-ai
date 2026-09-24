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
	if err := installAgentMemoryFile(tmpDir, rules, "/home/u/proj/.proj.worktrees", "", true); err != nil {
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
	//
	// "auto_allow" (lowercase) is the convention note, not the rendered
	// table — printRules upper-cases the action — so it pins the prose.
	for _, want := range []string{
		"Broker rules for this sandbox", "CONFIRM", "git-safe push", "gh pr create **",
		"auto_allow", "closed wrapper",
		// The worktree section must name the bound root, not the template.
		"Git worktrees", "/home/u/proj/.proj.worktrees",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("CLAUDE.md missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "{{worktree_root}}") {
		t.Errorf("CLAUDE.md still contains an unrendered placeholder:\n%s", s)
	}
}

// Without a bound worktree root there is nothing to name; the section must
// be absent rather than advertising a path that does not exist.
func TestInstallAgentMemoryFileNoWorktreeRoot(t *testing.T) {
	tmpDir := t.TempDir()
	if err := installAgentMemoryFile(tmpDir, nil, "", "", true); err != nil {
		t.Fatalf("installAgentMemoryFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmpDir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "Git worktrees") {
		t.Errorf("worktree section should be omitted without a root:\n%s", got)
	}
}

func TestInstallAgentMemoryFileNoRules(t *testing.T) {
	tmpDir := t.TempDir()
	if err := installAgentMemoryFile(tmpDir, nil, "", "", true); err != nil {
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

// A worktree start must name both writable locations when expose_main is
// on, and must record the hiding when it is off.
func TestRenderWorktreeSectionWorktreeStart(t *testing.T) {
	root := "/home/u/.proj.worktrees"
	main := "/home/u/proj"

	exposed := renderWorktreeSection(root, main, true)
	if !strings.Contains(exposed, "`"+root+"` and the main checkout at `"+main+"`") {
		t.Errorf("expose_main wording missing both roots:\n%s", exposed)
	}
	hidden := renderWorktreeSection(root, main, false)
	if !strings.Contains(hidden, "main checkout is not exposed") {
		t.Errorf("hidden-main wording missing:\n%s", hidden)
	}
	if strings.Contains(hidden, "`"+main+"`") {
		t.Errorf("hidden wording must not name the main tree:\n%s", hidden)
	}
	for _, s := range []string{exposed, hidden} {
		if strings.Contains(s, "{{") {
			t.Errorf("unrendered placeholder:\n%s", s)
		}
	}
}

func TestInstallOpencodeConfig(t *testing.T) {
	tmpDir := t.TempDir()
	if err := installOpencodeConfig(tmpDir); err != nil {
		t.Fatalf("installOpencodeConfig: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmpDir, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	// The fragment is passed to opencode via OPENCODE_CONFIG; its
	// instructions must point at the shared read-only context mount.
	for _, want := range []string{"/run/bwai/CLAUDE.md", "instructions"} {
		if !strings.Contains(s, want) {
			t.Errorf("opencode.json missing %q:\n%s", want, s)
		}
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
