package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPlanPush(t *testing.T) {
	cases := []struct {
		name         string
		branch       string
		remote       string
		remoteExists bool
		fastForward  bool
		hasUpstream  bool
		protected    []string
		want         []string
		wantErr      bool
	}{
		{
			name:    "detached head is refused",
			branch:  "",
			wantErr: true,
		},
		{
			name:    "protected main is refused",
			branch:  "main",
			wantErr: true,
		},
		{
			name:    "protected master is refused",
			branch:  "master",
			wantErr: true,
		},
		{
			name:    "protected branch is refused even when it does not exist remotely",
			branch:  "main",
			wantErr: true,
		},
		{
			name:         "non-fast-forward is refused",
			branch:       "feature",
			remoteExists: true,
			fastForward:  false,
			hasUpstream:  true,
			wantErr:      true,
		},
		{
			name:         "existing branch fast-forward with upstream",
			branch:       "feature",
			remoteExists: true,
			fastForward:  true,
			hasUpstream:  true,
			want:         []string{"git", "push", "origin", "HEAD:refs/heads/feature"},
		},
		{
			name:         "existing branch fast-forward without upstream sets it",
			branch:       "feature",
			remoteExists: true,
			fastForward:  true,
			hasUpstream:  false,
			want:         []string{"git", "push", "--set-upstream", "origin", "HEAD:refs/heads/feature"},
		},
		{
			name:   "new remote branch is created",
			branch: "feature",
			want:   []string{"git", "push", "--set-upstream", "origin", "HEAD:refs/heads/feature"},
		},
		{
			name:   "slash branch name is preserved",
			branch: "feat/deep-name",
			want:   []string{"git", "push", "--set-upstream", "origin", "HEAD:refs/heads/feat/deep-name"},
		},
		{
			name:   "an authorised non-origin remote flows into the argv",
			branch: "feature",
			remote: "backup",
			want:   []string{"git", "push", "--set-upstream", "backup", "HEAD:refs/heads/feature"},
		},
		{
			name:      "a configured glob refuses a matching branch",
			branch:    "release-1.0",
			protected: []string{"release-*"},
			wantErr:   true,
		},
		{
			name:      "a configured slash glob refuses a matching branch",
			branch:    "release/1.0",
			protected: []string{"release/*"},
			wantErr:   true,
		},
		{
			name:      "a branch outside the globs is still pushed",
			branch:    "feature",
			protected: []string{"release-*"},
			want:      []string{"git", "push", "--set-upstream", "origin", "HEAD:refs/heads/feature"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote := tc.remote
			if remote == "" {
				remote = "origin"
			}
			protected := tc.protected
			if protected == nil {
				protected = defaultProtectedBranches
			}
			got, err := planPush(tc.branch, remote, tc.remoteExists, tc.fastForward, tc.hasUpstream, protected)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("planPush(%q) = %v, want error", tc.branch, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("planPush(%q) = %v, want %v", tc.branch, got, tc.want)
			}
			// The whole point: no force, and no "+" refspec prefix.
			for _, tok := range got {
				if tok == "--force" || strings.HasPrefix(tok, "+") {
					t.Fatalf("planned argv contains a force form: %v", got)
				}
			}
		})
	}
}

func TestPlanPushRemote(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "default is origin", args: nil, want: "origin"},
		{name: "a bare name is accepted", args: []string{"backup"}, want: "backup"},
		{name: "flags are not remote names", args: []string{"--force"}, wantErr: true},
		{name: "a URL is not a remote name", args: []string{"https://github.com/o/r"}, wantErr: true},
		{name: "an scp URL is not a remote name", args: []string{"git@github.com:o/r.git"}, wantErr: true},
		{name: "a refspec is not a remote name", args: []string{"origin/main"}, wantErr: true},
		{name: "a force refspec is rejected", args: []string{"+main"}, wantErr: true},
		{name: "a dot is not a remote name", args: []string{"."}, wantErr: true},
		{name: "more than one remote is rejected", args: []string{"origin", "backup"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planPushRemote(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("planPushRemote(%v) = %q, want error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("planPushRemote(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestNormalizeRemoteURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"https", "https://github.com/CubeB/RWE-B4", "github.com/CubeB/RWE-B4"},
		{"https with .git", "https://github.com/CubeB/RWE-B4.git", "github.com/CubeB/RWE-B4"},
		{"https with trailing slash", "https://github.com/CubeB/RWE-B4/", "github.com/CubeB/RWE-B4"},
		{"https with userinfo", "https://user@github.com/CubeB/RWE-B4", "github.com/CubeB/RWE-B4"},
		{"ssh scheme", "ssh://git@github.com/CubeB/RWE-B4.git", "github.com/CubeB/RWE-B4"},
		{"ssh scheme with port", "ssh://git@github.com:22/CubeB/RWE-B4.git", "github.com/CubeB/RWE-B4"},
		{"scp syntax", "git@github.com:CubeB/RWE-B4.git", "github.com/CubeB/RWE-B4"},
		{"host case is folded, path is not", "git@GitHub.com:CubeB/RWE-B4.git", "github.com/CubeB/RWE-B4"},
		{"local path is returned as-is", "/tmp/x/origin.git", "/tmp/x/origin.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRemoteURL(tc.raw); got != tc.want {
				t.Fatalf("normalizeRemoteURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestURLAllowed(t *testing.T) {
	allowed := []string{"git@github.com:CubeB/RWE-B4.git"}
	// The same repository spelled three ways all match the one entry.
	for _, raw := range []string{
		"git@github.com:CubeB/RWE-B4.git",
		"https://github.com/CubeB/RWE-B4",
		"ssh://git@github.com/CubeB/RWE-B4.git",
	} {
		if !urlAllowed(normalizeRemoteURL(raw), allowed) {
			t.Errorf("%q should match the allowlist", raw)
		}
	}
	// A different repository does not, even on the same host.
	if urlAllowed(normalizeRemoteURL("git@github.com:attacker/exfil.git"), allowed) {
		t.Error("an unrelated repository must not match the allowlist")
	}
	// An empty allowlist allows nothing.
	if urlAllowed(normalizeRemoteURL("git@github.com:CubeB/RWE-B4.git"), nil) {
		t.Error("an empty allowlist must refuse everything")
	}
}

func TestBranchProtected(t *testing.T) {
	patterns := []string{"main", "release-*", "release/*", "hotfix/*"}
	cases := []struct {
		branch string
		want   bool
	}{
		{"main", true},
		{"release-1.0", true},
		{"release/1.0", true},
		{"release/1.0/x", false}, // "*" does not cross "/"
		{"hotfix/urgent", true},
		{"feature", false},
		{"mainline", false}, // exact names do not prefix-match
	}
	for _, tc := range cases {
		t.Run(tc.branch, func(t *testing.T) {
			if got := branchProtected(tc.branch, patterns); got != tc.want {
				t.Fatalf("branchProtected(%q) = %v, want %v", tc.branch, got, tc.want)
			}
		})
	}
	if branchProtected("anything", nil) {
		t.Fatal("no patterns must protect nothing")
	}
}

func TestRunGitSafeArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no subcommand", nil, 2},
		{"unknown subcommand", []string{"force-push"}, 2},
		{"push flags are rejected", []string{"push", "--force"}, 2},
		{"push refspec is rejected", []string{"push", "origin", "+main"}, 2},
		// Only argv shapes that fail before any git process is started
		// belong here — a valid `commit -m x` would commit in the test's
		// own working copy.
		// A well-formed commit is refused before any git process starts:
		// the host must never run git commit in the agent's repository.
		{"commit is retired", []string{"commit", "-m", "fix"}, 2},
		{"help", []string{"--help"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			silenceStdio(t, func() { code = runGitSafe(tc.args) })
			if code != tc.want {
				t.Fatalf("runGitSafe(%v) = %d, want %d", tc.args, code, tc.want)
			}
		})
	}
}

// TestGitSafePushIntegration drives the wrapper against a real repository
// and a local bare "origin".
func TestGitSafePushIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// Keep the host's git config (push.default, gpg signing, …) out of it.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	other := filepath.Join(root, "other.git")
	work := filepath.Join(root, "work")
	gitRun(t, root, "init", "--bare", "-b", "main", origin)
	gitRun(t, root, "init", "--bare", "-b", "main", other)
	gitRun(t, root, "init", "-b", "feature", work)
	gitRun(t, work, "remote", "add", "origin", origin)
	gitRun(t, work, "remote", "add", "backup", other)
	// The broker injects this snapshot; the tests stand in for it. origin is
	// the only authorised URL unless a subtest widens it.
	t.Setenv(pushAllowedEnv, origin)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "a.txt")
	gitRun(t, work, "commit", "-m", "a")

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	t.Run("creates the remote branch", func(t *testing.T) {
		if code := push(t); code != 0 {
			t.Fatalf("push exit %d, want 0", code)
		}
		if remoteSha(t, work, "origin", "feature") == "" {
			t.Fatal("origin/feature should exist after the push")
		}
		if up := strings.TrimSpace(gitRun(t, work, "rev-parse", "--abbrev-ref", "@{u}")); up != "origin/feature" {
			t.Fatalf("upstream = %q, want origin/feature", up)
		}
	})

	t.Run("refuses a remote that is not allowlisted", func(t *testing.T) {
		if code := pushTo(t, "backup"); code != 1 {
			t.Fatalf("non-allowlisted push exit %d, want 1", code)
		}
		if remoteSha(t, work, "backup", "feature") != "" {
			t.Fatal("backup/feature must not exist after a refused push")
		}
	})

	t.Run("refuses origin after it is retargeted off the allowlist", func(t *testing.T) {
		gitRun(t, work, "remote", "set-url", "origin", other)
		if code := push(t); code != 1 {
			t.Fatalf("retargeted-origin push exit %d, want 1", code)
		}
		gitRun(t, work, "remote", "set-url", "origin", origin)
	})

	t.Run("pushes to an allowlisted non-origin remote", func(t *testing.T) {
		t.Setenv(pushAllowedEnv, other)
		if code := pushTo(t, "backup"); code != 0 {
			t.Fatalf("allowlisted-backup push exit %d, want 0", code)
		}
		if remoteSha(t, work, "backup", "feature") == "" {
			t.Fatal("backup/feature should exist after the push")
		}
	})

	t.Run("refuses a non-fast-forward", func(t *testing.T) {
		before := remoteSha(t, work, "origin", "feature")
		// Rewrite the tip so the remote commit is no longer an ancestor.
		gitRun(t, work, "commit", "--amend", "-m", "a (amended)")
		if code := push(t); code != 1 {
			t.Fatalf("non-fast-forward push exit %d, want 1", code)
		}
		if after := remoteSha(t, work, "origin", "feature"); after != before {
			t.Fatalf("remote moved from %s to %s despite the refusal", before, after)
		}
	})

	t.Run("refuses a protected branch", func(t *testing.T) {
		gitRun(t, work, "checkout", "-b", "main")
		if code := push(t); code != 1 {
			t.Fatalf("protected-branch push exit %d, want 1", code)
		}
	})

	t.Run("refuses a configured protected-branch glob", func(t *testing.T) {
		t.Setenv(protectedBranchesEnv, "release-*")
		gitRun(t, work, "checkout", "-b", "release-1.0", "main")
		if code := push(t); code != 1 {
			t.Fatalf("protected-glob push exit %d, want 1", code)
		}
		if remoteSha(t, work, "origin", "release-1.0") != "" {
			t.Fatal("origin/release-1.0 must not exist after a refused push")
		}
	})

	t.Run("refuses a detached HEAD", func(t *testing.T) {
		gitRun(t, work, "checkout", "--detach")
		if code := push(t); code != 1 {
			t.Fatalf("detached-head push exit %d, want 1", code)
		}
	})
}

// TestRunGitSafeClient covers the sandbox-side half. It must never
// enforce anything itself: with no broker socket reachable it has to
// fail the round trip rather than fall back to running git locally,
// which is the property that keeps the policy on the host.
func TestRunGitSafeClient(t *testing.T) {
	t.Setenv("BWAI_BROKER_SOCKET", "")
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no subcommand", nil, 2},
		{"help is answered locally", []string{"--help"}, 0},
		{"forwarding without a broker fails", []string{"push"}, 127},
		{"commit is answered locally", []string{"commit", "-m", "x"}, 2},
		{"unknown subcommand still forwards", []string{"force-push"}, 127},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			silenceStdio(t, func() { code = runGitSafeClient(tc.args) })
			if code != tc.want {
				t.Fatalf("runGitSafeClient(%v) = %d, want %d", tc.args, code, tc.want)
			}
		})
	}
}

// TestRunGitSafeClientNamesItself guards the message an agent sees when
// a git-safe request is turned away: it has to name the command that was
// actually run, not the bwai-outside client that happens to implement it.
func TestRunGitSafeClientNamesItself(t *testing.T) {
	t.Setenv("BWAI_BROKER_SOCKET", "")
	old := outsideProg
	t.Cleanup(func() { outsideProg = old })

	outsideProg = "git-safe"
	var code int
	got := captureStderr(t, func() { code = runGitSafeClient([]string{"push"}) })
	if code != 127 {
		t.Fatalf("exit = %d, want 127", code)
	}
	if !strings.Contains(got, "git-safe: ") {
		t.Errorf("client should report as git-safe, got %q", got)
	}
	if strings.Contains(got, "bwai-outside") {
		t.Errorf("client should not claim to be bwai-outside, got %q", got)
	}
}

// captureStderr runs fn with os.Stderr pointed at a pipe and returns what
// was written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// push runs `git-safe push` with stdio silenced so the sandbox-facing
// output doesn't pollute the test log.
func push(t *testing.T) int {
	t.Helper()
	return pushTo(t, "")
}

// pushTo is push with an explicit remote; "" means the default (origin).
func pushTo(t *testing.T, remote string) int {
	t.Helper()
	var args []string
	if remote != "" {
		args = []string{remote}
	}
	var code int
	silenceStdio(t, func() { code = runGitSafePush(args) })
	return code
}

func remoteSha(t *testing.T, repo, remote, branch string) string {
	t.Helper()
	out := strings.TrimSpace(gitRun(t, repo, "ls-remote", remote, "refs/heads/"+branch))
	if out == "" {
		return ""
	}
	return strings.Fields(out)[0]
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// silenceStdio points os.Stdout/os.Stderr at /dev/null for the duration
// of fn, so CLI tests don't pollute the test output.
func silenceStdio(t *testing.T, fn func()) {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, devNull
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	fn()
}
