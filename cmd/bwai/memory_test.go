package main

import (
	"os"
	"os/exec"
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
	if err := installAgentMemoryFile(tmpDir, rules, "/home/u/proj/.proj.worktrees", "", "", "", true); err != nil {
		t.Fatalf("installAgentMemoryFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmpDir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.HasPrefix(s, brokerBlockBegin+"\n"+strings.Replace(agentMemoryFileContent, "{{intro}}", sandboxIntro, 1)) {
		t.Errorf("CLAUDE.md should open with the bwai fragment:\n%s", s)
	}
	if !strings.HasSuffix(s, brokerBlockEnd+"\n") {
		t.Errorf("CLAUDE.md should close the managed block:\n%s", s)
	}
	// The rules ride along in the fragment so the agent knows them before
	// its first turn, without having to run `bwai-outside --list-rules`.
	//
	// "auto_allow" (lowercase) is the convention note, not the rendered
	// table — printRules upper-cases the action — so it pins the prose.
	for _, want := range []string{
		"## Broker rules", "inside a bwai sandbox", "CONFIRM", "git-safe push", "gh pr create **",
		"auto_allow", "closed wrapper",
		// The worktree section must name the bound root, not the template.
		"Git worktrees", "/home/u/proj/.proj.worktrees",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("CLAUDE.md missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "{{worktree_root}}") || strings.Contains(s, "{{intro}}") {
		t.Errorf("CLAUDE.md still contains an unrendered placeholder:\n%s", s)
	}
	codex, err := os.ReadFile(filepath.Join(tmpDir, "CODEX_AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(codex) != s {
		t.Errorf("Codex broker guidance differs from shared agent guidance")
	}
	cline, err := os.ReadFile(filepath.Join(tmpDir, "CLINE_RULES.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cline) != s {
		t.Errorf("Cline broker guidance differs from shared agent guidance")
	}
}

func TestCodexHomePath(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	if got := codexHomePath(home, project, defaultConfig()); got != filepath.Join(home, ".codex") {
		t.Errorf("default codex home = %q, want %q", got, filepath.Join(home, ".codex"))
	}
	cfg := defaultConfig()
	cfg.EnvSet["CODEX_HOME"] = "~/.local/share/bwai/codex"
	want := filepath.Join(home, ".local/share/bwai/codex")
	if got := codexHomePath(home, project, cfg); got != want {
		t.Errorf("configured codex home = %q, want %q", got, want)
	}
	cfg = defaultConfig()
	cfg.EnvSet["CODEX_HOME"] = "codex-state"
	if got, want := codexHomePath(home, project, cfg), filepath.Join(project, "codex-state"); got != want {
		t.Errorf("relative codex home = %q, want %q", got, want)
	}
}

func TestCodexHomeWritable(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	project := filepath.Join(root, "project")
	state := filepath.Join(root, "state")
	for _, dir := range []string{home, project, state, filepath.Join(home, ".codex")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := defaultConfig()
	cfg.HomeAllow = nil
	if codexHomeWritable(filepath.Join(home, ".codex"), home, project, state, cfg) {
		t.Fatal("read-only CODEX_HOME should not be treated as writable")
	}
	cfg.HomeAllow = []string{".codex"}
	if !codexHomeWritable(filepath.Join(home, ".codex"), home, project, state, cfg) {
		t.Fatal("allowed CODEX_HOME should be writable")
	}
	if !codexHomeWritable(filepath.Join(state, "codex"), home, project, state, cfg) {
		t.Fatal("CODEX_HOME under state_root should be writable")
	}
	if !codexHomeWritable(filepath.Join(project, "codex"), home, project, state, cfg) {
		t.Fatal("CODEX_HOME under project should be writable")
	}
}

// Without a bound worktree root there is nothing to name; the section must
// be absent rather than advertising a path that does not exist.
func TestInstallAgentMemoryFileNoWorktreeRoot(t *testing.T) {
	tmpDir := t.TempDir()
	if err := installAgentMemoryFile(tmpDir, nil, "", "", "", "", true); err != nil {
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
	if err := installAgentMemoryFile(tmpDir, nil, "", "", "", "", true); err != nil {
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

func TestInstallOpencodeAgents(t *testing.T) {
	tmpDir := t.TempDir()
	host := filepath.Join(t.TempDir(), "AGENTS.md")
	fragment := []byte(brokerBlockBegin + "\n# bwai broker\nrules\n" + brokerBlockEnd + "\n")
	if err := os.WriteFile(host, []byte("# host instructions\nkeep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installOpencodeAgents(tmpDir, host, fragment); err != nil {
		t.Fatalf("installOpencodeAgents: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmpDir, "OPENCODE_AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	// opencode v2 reads only AGENTS.md, so the host's own global
	// instructions must survive alongside the broker fragment.
	if s := string(got); !strings.Contains(s, "keep me") || !strings.Contains(s, "# bwai broker") {
		t.Errorf("merged AGENTS.md missing host or broker content:\n%s", s)
	}
	// On an agent box the host file carries a marked bwai block. A refresh
	// replaces that block in place, keeping the host's own instructions and
	// anything the user wrote after the block.
	hostContent := "my rules\n\n" + brokerBlockBegin + "\n# bwai broker\nstale\n" + brokerBlockEnd + "\n\ntrailing note\n"
	if err := os.WriteFile(host, []byte(hostContent), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := []byte(brokerBlockBegin + "\n# bwai broker\nfresh\n" + brokerBlockEnd + "\n")
	if err := installOpencodeAgents(tmpDir, host, fresh); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(tmpDir, "OPENCODE_AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "stale") || !strings.Contains(s, "fresh") {
		t.Errorf("agent-box AGENTS.md should be replaced, not merged:\n%s", s)
	}
	if !strings.Contains(s, "my rules") || !strings.Contains(s, "trailing note") {
		t.Errorf("host content around the block must survive:\n%s", s)
	}
	if n := strings.Count(s, brokerBlockBegin); n != 1 {
		t.Errorf("want exactly one managed block, got %d:\n%s", n, s)
	}
	// A legacy agent-box file that is only the old unmarked fragment is
	// replaced wholesale rather than getting a block appended.
	if err := os.WriteFile(host, []byte("# bwai broker\nstale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installOpencodeAgents(tmpDir, host, fresh); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(tmpDir, "OPENCODE_AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got); strings.Contains(s, "stale") || !strings.Contains(s, "fresh") {
		t.Errorf("legacy agent-box AGENTS.md should be replaced:\n%s", s)
	}
}

func TestInstallClineRules(t *testing.T) {
	tmpDir := t.TempDir()
	fragment := []byte(brokerBlockBegin + "\n# bwai broker\nrules\n" + brokerBlockEnd + "\n")
	// Without a host rule file the fragment stands alone.
	if err := installClineRules(tmpDir, filepath.Join(t.TempDir(), "bwai.md"), fragment); err != nil {
		t.Fatalf("installClineRules: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tmpDir, "CLINE_RULES.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(fragment) {
		t.Errorf("standalone rule file = %q, want the fragment", got)
	}
	// A host rule file of the same name — the user's own, or one an agent
	// box wrote — is merged rather than shadowed, and its marked block is
	// replaced in place.
	host := filepath.Join(t.TempDir(), "bwai.md")
	hostContent := "my rules\n\n" + brokerBlockBegin + "\n# bwai broker\nstale\n" + brokerBlockEnd + "\n"
	if err := os.WriteFile(host, []byte(hostContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installClineRules(tmpDir, host, fragment); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(tmpDir, "CLINE_RULES.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "my rules") {
		t.Errorf("host rules must survive the merge:\n%s", s)
	}
	if strings.Contains(s, "stale") || !strings.Contains(s, "# bwai broker") {
		t.Errorf("the marked block should be replaced with the fragment:\n%s", s)
	}
}

func TestOpencodeAgentsMountable(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	// A writable home_allow sub-path lets bwrap create the mountpoint.
	if !opencodeAgentsMountable(dir, home, cfg) {
		t.Fatal("allowed opencode dir should be mountable")
	}
	// A missing directory under the read-only home overlay cannot host one.
	if opencodeAgentsMountable(filepath.Join(home, ".config", "absent"), home, cfg) {
		t.Fatal("missing dir should not be mountable")
	}
	// An existing file is a mountpoint even when the parent is read-only.
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.HomeAllow = nil
	if !opencodeAgentsMountable(dir, home, cfg) {
		t.Fatal("existing AGENTS.md should be mountable")
	}
}

func TestClineRulesMountable(t *testing.T) {
	home := t.TempDir()
	path := clineRulesPath(home)
	cfg := defaultConfig()
	// A host with no ~/.cline yet gets it on the writable home overlay.
	if !clineRulesMountable(path, home, cfg) {
		t.Fatal("a host without ~/.cline should be mountable")
	}
	// An existing ~/.cline is mounted read-write only when home_allow names
	// it, and bwrap cannot create a mountpoint under a read-only parent.
	if err := os.MkdirAll(filepath.Join(home, ".cline"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !clineRulesMountable(path, home, cfg) {
		t.Fatal("allowed ~/.cline should be mountable")
	}
	cfg.HomeAllow = nil
	if clineRulesMountable(path, home, cfg) {
		t.Fatal("read-only ~/.cline should not be mountable")
	}
	// A rule file that already exists is its own mountpoint, whatever the
	// parent allows.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !clineRulesMountable(path, home, cfg) {
		t.Fatal("an existing rule file should be mountable")
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

// agentBoxRefreshScript is the box-side counterpart to
// installAgentMemoryFile: on an agent box bwai owns the agent user's home,
// so bwai-refresh-context writes each agent's global instructions there
// instead of overlaying them inside a sandbox.
func agentBoxRefreshScript(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "scripts", "agent-box", "bwai-refresh-context"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAgentBoxRefreshCoversEveryAgent guards the drift that leaves an agent
// box without broker guidance: an agent whose global instructions a sandbox
// wires up has to appear in the box's refresh script too. The paths come
// from the sandbox's own helpers, so moving one fails here until the script
// follows it.
func TestAgentBoxRefreshCoversEveryAgent(t *testing.T) {
	script, err := os.ReadFile(agentBoxRefreshScript(t))
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(string(filepath.Separator), "home", "agent")
	for _, path := range []string{
		opencodeAgentsPath(home),                         // opencode v2: the global AGENTS.md
		clineRulesPath(home),                             // Cline: one file among its global rules
		filepath.Join(home, ".claude", "CLAUDE.md"),      // Claude Code
		filepath.Join(home, ".codex", "AGENTS.md"),       // Codex: AGENTS.md in CODEX_HOME
		filepath.Join(home, ".commandcode", "AGENTS.md"), // command-code: user memory
	} {
		rel, err := filepath.Rel(home, path)
		if err != nil {
			t.Fatal(err)
		}
		want := `"$HOME/` + filepath.ToSlash(rel) + `"`
		if !strings.Contains(string(script), want) {
			t.Errorf("bwai-refresh-context never writes %s (%s), so that agent gets no broker guidance on a box",
				want, path)
		}
	}
}

// TestAgentBoxRefreshContextWritesEveryAgent runs the box's refresh script
// against a stub client and a throwaway home. Each agent's global
// instructions file ends up with the broker block, the agent user's own
// content survives, a file bwai wrote before the block was marked is
// replaced, and a second login rewrites the block rather than repeating it.
func TestAgentBoxRefreshContextWritesEveryAgent(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script := agentBoxRefreshScript(t)
	home := t.TempDir()
	bin := t.TempDir()
	// The script only checks for the markers and copies what the client
	// prints, so a stub can stand in for bwai-outside.
	stub := filepath.Join(bin, "bwai-outside")
	body := "#!/usr/bin/env bash\n[[ ${1:-} == --context ]] || exit 1\nprintf '%s\\n' '" +
		brokerBlockBegin + "' 'call bwai-outside for host credentials' '" + brokerBlockEnd + "'\n"
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	// The agent user's own instructions, and a whole file bwai wrote before
	// the block was marked (its first line is the fragment's heading).
	own := opencodeAgentsPath(home)
	legacy := filepath.Join(home, ".codex", "AGENTS.md")
	for path, content := range map[string]string{
		own:    "# my rules\n",
		legacy: "# bwai broker\nstale guidance\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Two logins: the second must leave one block, not two.
	for i := 0; i < 2; i++ {
		cmd := exec.Command("bash", script)
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":" + os.Getenv("PATH")}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("refresh run %d: %v\n%s", i+1, err, out)
		}
	}

	for _, path := range []string{
		filepath.Join(home, ".claude", "CLAUDE.md"),
		clineRulesPath(home),
		filepath.Join(home, ".commandcode", "AGENTS.md"),
		own,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if n := strings.Count(string(got), brokerBlockBegin); n != 1 {
			t.Errorf("%s has %d broker blocks, want 1:\n%s", path, n, got)
		}
		if !strings.Contains(string(got), "call bwai-outside for host credentials") {
			t.Errorf("%s missing the broker block:\n%s", path, got)
		}
	}
	if got, err := os.ReadFile(own); err != nil || !strings.Contains(string(got), "# my rules") {
		t.Errorf("the agent user's own instructions were lost (err %v):\n%s", err, got)
	}
	if got, err := os.ReadFile(legacy); err != nil || strings.Contains(string(got), "stale guidance") {
		t.Errorf("a file bwai owns should be replaced wholesale (err %v):\n%s", err, got)
	}
}
