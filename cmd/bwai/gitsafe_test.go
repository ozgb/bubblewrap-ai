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
	const dest = "git@github.com:o/r.git"
	const sha = "0123456789abcdef0123456789abcdef01234567"
	push := func(branch string) []string {
		return []string{"git", "push", dest, sha + ":refs/heads/" + branch}
	}
	cases := []struct {
		name         string
		branch       string
		remoteExists bool
		fastForward  bool
		protected    []string
		want         []string
		wantErr      bool
	}{
		{name: "detached head is refused", branch: "", wantErr: true},
		{name: "protected main is refused", branch: "main", wantErr: true},
		{name: "protected master is refused", branch: "master", wantErr: true},
		{
			name:         "non-fast-forward is refused",
			branch:       "feature",
			remoteExists: true,
			wantErr:      true,
		},
		{
			name:         "existing branch fast-forward",
			branch:       "feature",
			remoteExists: true,
			fastForward:  true,
			want:         push("feature"),
		},
		{name: "new remote branch is created", branch: "feature", want: push("feature")},
		{name: "slash branch name is preserved", branch: "feat/deep-name", want: push("feat/deep-name")},
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
			want:      push("feature"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			protected := tc.protected
			if protected == nil {
				protected = defaultProtectedBranches
			}
			got, err := planPush(tc.branch, "origin", dest, sha, tc.remoteExists, tc.fastForward, protected)
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
		if allowedEntry(normalizeRemoteURL(raw), allowed) == "" {
			t.Errorf("%q should match the allowlist", raw)
		}
	}
	// A different repository does not, even on the same host.
	if allowedEntry(normalizeRemoteURL("git@github.com:attacker/exfil.git"), allowed) != "" {
		t.Error("an unrelated repository must not match the allowlist")
	}
	// An empty allowlist allows nothing.
	if allowedEntry(normalizeRemoteURL("git@github.com:CubeB/RWE-B4.git"), nil) != "" {
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
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	other := filepath.Join(root, "other.git")
	work := filepath.Join(root, "work")
	gitRun(t, root, "init", "--bare", "-b", "main", origin)
	gitRun(t, root, "init", "--bare", "-b", "main", other)
	gitRun(t, root, "init", "-b", "feature", work)
	gitRun(t, work, "remote", "add", "origin", origin)
	gitRun(t, work, "remote", "add", "backup", other)
	// The broker injects these; the tests stand in for it. origin is the
	// only authorised URL unless a subtest widens it.
	t.Setenv(pushAllowedEnv, origin)
	t.Setenv(requestCwdEnv, work)
	t.Setenv(allowedRootsEnv, root)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "a.txt")
	gitRun(t, work, "commit", "-m", "a")

	t.Run("creates the remote branch", func(t *testing.T) {
		if code := push(t); code != 0 {
			t.Fatalf("push exit %d, want 0", code)
		}
		if got, want := remoteSha(t, work, "origin", "feature"), headSha(t, work); got != want {
			t.Fatalf("origin/feature = %q, want HEAD %q", got, want)
		}
	})

	t.Run("fast-forwards an existing branch", func(t *testing.T) {
		gitRun(t, work, "commit", "--allow-empty", "-m", "b")
		if code := push(t); code != 0 {
			t.Fatalf("push exit %d, want 0", code)
		}
		if got, want := remoteSha(t, work, "origin", "feature"), headSha(t, work); got != want {
			t.Fatalf("origin/feature = %q, want HEAD %q", got, want)
		}
	})

	t.Run("never runs the repository's hooks or config", func(t *testing.T) {
		marker := filepath.Join(root, "pwned")
		evil := filepath.Join(root, "evil.sh")
		writeScript(t, evil, "touch "+marker+"\n")
		hooks := filepath.Join(work, ".git", "hooks")
		for _, h := range []string{"pre-push", "pre-commit", "post-checkout", "reference-transaction", "post-update"} {
			writeScript(t, filepath.Join(hooks, h), "touch "+marker+"\n")
		}
		gitRun(t, work, "config", "core.hooksPath", hooks)
		gitRun(t, work, "config", "core.fsmonitor", evil)
		gitRun(t, work, "config", "core.sshCommand", evil)
		gitRun(t, work, "config", "uploadpack.packObjectsHook", evil)
		gitRun(t, work, "config", "filter.x.clean", evil)
		gitRun(t, work, "config", "filter.x.smudge", evil)
		gitRun(t, work, "config", "remote.origin.receivepack", evil)
		if err := os.WriteFile(filepath.Join(work, ".gitattributes"), []byte("* filter=x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, work, "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "c")
		_ = os.Remove(marker)
		t.Cleanup(func() {
			for _, k := range []string{"core.hooksPath", "core.fsmonitor", "core.sshCommand", "uploadpack.packObjectsHook", "filter.x.clean", "filter.x.smudge", "remote.origin.receivepack"} {
				gitRun(t, work, "config", "--unset", k)
			}
			_ = os.Remove(filepath.Join(work, ".gitattributes"))
		})

		if code := push(t); code != 0 {
			t.Fatalf("push exit %d, want 0", code)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("pushing ran code planted in the repository")
		}
		if got, want := remoteSha(t, work, "origin", "feature"), headSha(t, work); got != want {
			t.Fatalf("origin/feature = %q, want HEAD %q", got, want)
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
		gitRun(t, work, "commit", "--amend", "--allow-empty", "-m", "c (amended)")
		if code := push(t); code != 1 {
			t.Fatalf("non-fast-forward push exit %d, want 1", code)
		}
		if after := remoteSha(t, work, "origin", "feature"); after != before {
			t.Fatalf("remote moved from %s to %s despite the refusal", before, after)
		}
	})

	t.Run("refuses a git dir outside the roots", func(t *testing.T) {
		t.Setenv(allowedRootsEnv, filepath.Join(root, "elsewhere"))
		if code := push(t); code != 1 {
			t.Fatalf("out-of-roots push exit %d, want 1", code)
		}
	})

	t.Run("refuses a .git pointer out of the roots", func(t *testing.T) {
		outside := t.TempDir()
		gitRun(t, outside, "init", "-b", "feature", "victim")
		linked := filepath.Join(root, "linked")
		if err := os.MkdirAll(linked, 0o755); err != nil {
			t.Fatal(err)
		}
		pointer := "gitdir: " + filepath.Join(outside, "victim", ".git") + "\n"
		if err := os.WriteFile(filepath.Join(linked, ".git"), []byte(pointer), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv(requestCwdEnv, linked)
		if code := push(t); code != 1 {
			t.Fatalf("pointer-escape push exit %d, want 1", code)
		}
	})

	t.Run("refuses object alternates", func(t *testing.T) {
		alt := filepath.Join(work, ".git", "objects", "info", "alternates")
		if err := os.WriteFile(alt, []byte(filepath.Join(origin, "objects")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(alt) })
		if code := push(t); code != 1 {
			t.Fatalf("alternates push exit %d, want 1", code)
		}
	})

	t.Run("refuses a protected branch", func(t *testing.T) {
		gitRun(t, work, "checkout", "-q", "-b", "main")
		if code := push(t); code != 1 {
			t.Fatalf("protected-branch push exit %d, want 1", code)
		}
	})

	t.Run("refuses a configured protected-branch glob", func(t *testing.T) {
		t.Setenv(protectedBranchesEnv, "release-*")
		gitRun(t, work, "checkout", "-q", "-b", "release-1.0", "main")
		if code := push(t); code != 1 {
			t.Fatalf("protected-glob push exit %d, want 1", code)
		}
		if remoteSha(t, work, "origin", "release-1.0") != "" {
			t.Fatal("origin/release-1.0 must not exist after a refused push")
		}
	})

	t.Run("refuses a detached HEAD", func(t *testing.T) {
		gitRun(t, work, "checkout", "-q", "--detach")
		if code := push(t); code != 1 {
			t.Fatalf("detached-head push exit %d, want 1", code)
		}
	})
}

func headSha(t *testing.T, repo string) string {
	t.Helper()
	return strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))
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
