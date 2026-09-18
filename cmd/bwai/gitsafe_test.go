package main

import (
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
