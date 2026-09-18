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
		remoteExists bool
		fastForward  bool
		hasUpstream  bool
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planPush(tc.branch, tc.remoteExists, tc.fastForward, tc.hasUpstream)
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

func TestPlanCommit(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    []string
		wantErr bool
	}{
		{
			name:    "no message",
			args:    nil,
			wantErr: true,
		},
		{
			name:    "dangling -m",
			args:    []string{"-m"},
			wantErr: true,
		},
		{
			name:    "amend is refused",
			args:    []string{"-m", "fix", "--amend"},
			wantErr: true,
		},
		{
			name:    "staging shorthand is refused",
			args:    []string{"-a", "-m", "fix"},
			wantErr: true,
		},
		{
			name:    "signing is not the caller's choice",
			args:    []string{"--no-gpg-sign", "-m", "fix"},
			wantErr: true,
		},
		{
			name:    "pathspec is refused",
			args:    []string{"-m", "fix", "a.txt"},
			wantErr: true,
		},
		{
			name:    "message file is refused",
			args:    []string{"-F", "-"},
			wantErr: true,
		},
		{
			name: "single message",
			args: []string{"-m", "fix bug"},
			want: []string{"git", "commit", "-S", "-m", "fix bug"},
		},
		{
			name: "repeated -m adds paragraphs",
			args: []string{"-m", "subject", "-m", "body"},
			want: []string{"git", "commit", "-S", "-m", "subject", "-m", "body"},
		},
		{
			name: "multiline message is one argument",
			args: []string{"-m", "subject\n\nbody"},
			want: []string{"git", "commit", "-S", "-m", "subject\n\nbody"},
		},
		{
			name: "a message that looks like a flag stays a message",
			args: []string{"-m", "--amend"},
			want: []string{"git", "commit", "-S", "-m", "--amend"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planCommit(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("planCommit(%v) = %v, want error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("planCommit(%v) = %v, want %v", tc.args, got, tc.want)
			}
			// The whole point: signing is not the caller's choice.
			if !slices.Contains(got, "-S") {
				t.Fatalf("planned argv is not signed: %v", got)
			}
		})
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
		{"commit without a message", []string{"commit"}, 2},
		{"commit with an amend", []string{"commit", "-m", "fix", "--amend"}, 2},
		{"commit with a pathspec", []string{"commit", "-m", "fix", "a.txt"}, 2},
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
	work := filepath.Join(root, "work")
	gitRun(t, root, "init", "--bare", "-b", "main", origin)
	gitRun(t, root, "init", "-b", "feature", work)
	gitRun(t, work, "remote", "add", "origin", origin)
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
		if remoteSha(t, work, "feature") == "" {
			t.Fatal("origin/feature should exist after the push")
		}
		if up := strings.TrimSpace(gitRun(t, work, "rev-parse", "--abbrev-ref", "@{u}")); up != "origin/feature" {
			t.Fatalf("upstream = %q, want origin/feature", up)
		}
	})

	t.Run("refuses a non-fast-forward", func(t *testing.T) {
		before := remoteSha(t, work, "feature")
		// Rewrite the tip so the remote commit is no longer an ancestor.
		gitRun(t, work, "commit", "--amend", "-m", "a (amended)")
		if code := push(t); code != 1 {
			t.Fatalf("non-fast-forward push exit %d, want 1", code)
		}
		if after := remoteSha(t, work, "feature"); after != before {
			t.Fatalf("remote moved from %s to %s despite the refusal", before, after)
		}
	})

	t.Run("refuses a protected branch", func(t *testing.T) {
		gitRun(t, work, "checkout", "-b", "main")
		if code := push(t); code != 1 {
			t.Fatalf("protected-branch push exit %d, want 1", code)
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

// TestGitSafeCommitIntegration drives `git-safe commit` against a real
// repository. The wrapper always signs, so the test installs a stub
// gpg.program: git commit -S pipes the commit buffer to the program and
// embeds whatever carries the PGP markers back out. That keeps the test
// hermetic — no real keyring, no key generation.
func TestGitSafeCommitIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	root := t.TempDir()
	work := filepath.Join(root, "work")
	gitRun(t, root, "init", "-b", "feature", work)

	gpg := filepath.Join(root, "fake-gpg")
	// git -S reads the detached signature from stdout and, on recent git,
	// insists on the [GNUPG:] SIG_CREATED status line on stderr before it
	// will accept it. The payload is not verified, so a marker pair and a
	// status line are all the stub needs.
	stub := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"echo '-----BEGIN PGP SIGNATURE-----'\n" +
		"echo\n" +
		"echo dGVzdA==\n" +
		"echo '-----END PGP SIGNATURE-----'\n" +
		"echo '[GNUPG:] SIG_CREATED D 1 8 00 1700000000 0123456789ABCDEF0123456789ABCDEF01234567' >&2\n"
	if err := os.WriteFile(gpg, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "config", "gpg.program", gpg)
	gitRun(t, work, "config", "user.signingkey", "test")
	gitRun(t, work, "config", "user.name", "Test")
	gitRun(t, work, "config", "user.email", "test@example.com")

	// Stage a file, then hand control to the wrapper.
	stage := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, work, "add", name)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	commit := func(args ...string) int {
		t.Helper()
		var code int
		silenceStdio(t, func() { code = runGitSafeCommit(args) })
		return code
	}

	t.Run("commits staged changes, signed", func(t *testing.T) {
		stage("a.txt", "a\n")
		if code := commit("-m", "subject", "-m", "body"); code != 0 {
			t.Fatalf("commit exit %d, want 0", code)
		}
		if n := strings.TrimSpace(gitRun(t, work, "rev-list", "--count", "HEAD")); n != "1" {
			t.Fatalf("commit count = %s, want 1", n)
		}
		// -S reached git: the commit object carries a gpgsig header.
		if obj := gitRun(t, work, "cat-file", "-p", "HEAD"); !strings.Contains(obj, "gpgsig") {
			t.Fatalf("commit is not signed:\n%s", obj)
		}
		// Repeated -m became paragraphs, as git would have.
		if msg := gitRun(t, work, "log", "-1", "--format=%B"); !strings.Contains(msg, "subject\n\nbody") {
			t.Fatalf("commit message = %q, want the two paragraphs", msg)
		}
	})

	t.Run("refuses a message-less commit", func(t *testing.T) {
		stage("b.txt", "b\n")
		if code := commit(); code != 2 {
			t.Fatalf("commit exit %d, want 2", code)
		}
	})

	t.Run("refuses --amend", func(t *testing.T) {
		if code := commit("-m", "rewrite", "--amend"); code != 2 {
			t.Fatalf("commit exit %d, want 2", code)
		}
		if msg := gitRun(t, work, "log", "-1", "--format=%s"); !strings.Contains(msg, "subject") {
			t.Fatalf("HEAD moved despite the refusal: %q", msg)
		}
	})

	t.Run("refuses a pathspec", func(t *testing.T) {
		if code := commit("-m", "just a.txt", "a.txt"); code != 2 {
			t.Fatalf("commit exit %d, want 2", code)
		}
	})

	t.Run("refuses a detached HEAD", func(t *testing.T) {
		gitRun(t, work, "checkout", "--detach")
		if code := commit("-m", "orphan"); code != 1 {
			t.Fatalf("detached-head commit exit %d, want 1", code)
		}
	})
}

// push runs `git-safe push` with stdio silenced so the sandbox-facing
// output doesn't pollute the test log.
func push(t *testing.T) int {
	t.Helper()
	var code int
	silenceStdio(t, func() { code = runGitSafePush(nil) })
	return code
}

func remoteSha(t *testing.T, repo, branch string) string {
	t.Helper()
	out := strings.TrimSpace(gitRun(t, repo, "ls-remote", "origin", "refs/heads/"+branch))
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
