package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPlanPushTagArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		remote     string
		tag        string
		wantErrSub string
	}{
		{"tag only", []string{"v1.0"}, "origin", "v1.0", ""},
		{"remote and tag", []string{"backup", "v1.0"}, "backup", "v1.0", ""},
		{"nested tag", []string{"release/2.0"}, "origin", "release/2.0", ""},
		{"no args", nil, "", "", "expected [<remote>] <tag>"},
		{"too many", []string{"origin", "v1", "v2"}, "", "", "expected [<remote>] <tag>"},
		{"flag as tag", []string{"--force"}, "", "", "not a valid tag name"},
		{"refspec as tag", []string{"+v1.0"}, "", "", "not a valid tag name"},
		{"delete refspec", []string{":v1.0"}, "", "", "not a valid tag name"},
		{"flag as remote", []string{"--force", "v1.0"}, "", "", "not a valid remote name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			remote, tag, err := planPushTagArgs(tc.args)
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil || remote != tc.remote || tag != tc.tag {
				t.Fatalf("got (%q, %q, %v), want (%q, %q)", remote, tag, err, tc.remote, tc.tag)
			}
		})
	}
}

func TestPlanPushTag(t *testing.T) {
	const dest = "git@github.com:o/r.git"
	pats := []string{"v*"}
	cases := []struct {
		name        string
		tag         string
		remoteObj   string
		onProtected bool
		patterns    []string
		want        []string
		wantErrSub  string
	}{
		{"new tag on a protected commit", "v1.0", "", true, pats,
			[]string{"git", "push", dest, "abc:refs/tags/v1.0"}, ""},
		{"already pushed is a no-op", "v1.0", "abc", true, pats, nil, ""},
		{"never moves a tag", "v1.0", "def", true, pats, nil, "never moved"},
		{"commit not on a protected branch", "v1.0", "", false, pats, nil, "not on a protected branch"},
		{"name outside the patterns", "nightly", "", true, pats, nil, "tag_patterns"},
		{"no patterns allows nothing", "v1.0", "", true, nil, nil, "tag_patterns"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planPushTag(tc.tag, "origin", dest, "abc", tc.remoteObj, tc.onProtected, tc.patterns)
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got (%v, %v), want %v", got, err, tc.want)
			}
		})
	}
}

// TestGitSafePushTagIntegration drives push-tag against a real repository
// and a local bare "origin" whose main stands in for a reviewed branch.
func TestGitSafePushTagIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	gitRun(t, root, "init", "--bare", "-b", "main", origin)
	gitRun(t, root, "init", "-b", "main", work)
	gitRun(t, work, "remote", "add", "origin", origin)
	setAllowedRemotes(t, AllowedRemote{URL: origin, TagPatterns: []string{"v*"}})
	t.Setenv(requestCwdEnv, work)
	t.Setenv(allowedRootsEnv, root)

	gitRun(t, work, "commit", "--allow-empty", "-m", "released")
	// A human merge: the agent's own git-safe push cannot reach main.
	gitRun(t, work, "push", "--quiet", "origin", "main")

	t.Run("pushes a lightweight tag on main", func(t *testing.T) {
		gitRun(t, work, "tag", "v1.0")
		if code := pushTag(t, "v1.0"); code != 0 {
			t.Fatalf("push-tag exit %d, want 0", code)
		}
		if got, want := remoteTag(t, work, "v1.0"), headSha(t, work); got != want {
			t.Fatalf("origin v1.0 = %q, want %q", got, want)
		}
	})

	t.Run("pushes an annotated tag as the tag object", func(t *testing.T) {
		gitRun(t, work, "tag", "-a", "v1.1", "-m", "v1.1")
		if code := pushTag(t, "v1.1"); code != 0 {
			t.Fatalf("push-tag exit %d, want 0", code)
		}
		want := strings.TrimSpace(gitRun(t, work, "rev-parse", "refs/tags/v1.1"))
		if got := remoteTag(t, work, "v1.1"); got != want {
			t.Fatalf("origin v1.1 = %q, want tag object %q", got, want)
		}
	})

	t.Run("pushing the same tag again is a no-op", func(t *testing.T) {
		if code := pushTag(t, "v1.0"); code != 0 {
			t.Fatalf("repeat push-tag exit %d, want 0", code)
		}
	})

	t.Run("never moves a published tag", func(t *testing.T) {
		before := remoteTag(t, work, "v1.0")
		gitRun(t, work, "commit", "--allow-empty", "-m", "next")
		gitRun(t, work, "push", "--quiet", "origin", "main")
		gitRun(t, work, "tag", "-f", "v1.0")
		if code := pushTag(t, "v1.0"); code != 1 {
			t.Fatalf("moved push-tag exit %d, want 1", code)
		}
		if got := remoteTag(t, work, "v1.0"); got != before {
			t.Fatalf("origin v1.0 moved to %q", got)
		}
	})

	t.Run("refuses a commit that is not on a protected branch", func(t *testing.T) {
		gitRun(t, work, "checkout", "-q", "-b", "feature")
		gitRun(t, work, "commit", "--allow-empty", "-m", "unreviewed")
		gitRun(t, work, "push", "--quiet", "origin", "feature")
		gitRun(t, work, "tag", "v2.0")
		if code := pushTag(t, "v2.0"); code != 1 {
			t.Fatalf("unreviewed push-tag exit %d, want 1", code)
		}
		if remoteTag(t, work, "v2.0") != "" {
			t.Fatal("origin v2.0 must not exist after a refused push")
		}
		gitRun(t, work, "checkout", "-q", "main")
	})

	t.Run("a remote's own protected branches count", func(t *testing.T) {
		setAllowedRemotes(t, AllowedRemote{URL: origin, TagPatterns: []string{"v*"}, ProtectedBranches: []string{"feature"}})
		if code := pushTag(t, "v2.0"); code != 0 {
			t.Fatalf("push-tag on a configured protected branch exit %d, want 0", code)
		}
	})

	t.Run("refuses a name outside tag_patterns", func(t *testing.T) {
		gitRun(t, work, "tag", "nightly")
		if code := pushTag(t, "nightly"); code != 1 {
			t.Fatalf("unmatched push-tag exit %d, want 1", code)
		}
	})

	t.Run("refuses every tag when tag_patterns is empty", func(t *testing.T) {
		setAllowedRemotes(t, AllowedRemote{URL: origin})
		gitRun(t, work, "tag", "v3.0")
		if code := pushTag(t, "v3.0"); code != 1 {
			t.Fatalf("empty-patterns push-tag exit %d, want 1", code)
		}
	})

	t.Run("refuses an annotated tag published under another name", func(t *testing.T) {
		gitRun(t, work, "tag", "-a", "v4.0", "-m", "v4.0")
		gitRun(t, work, "update-ref", "refs/tags/v4.1", "refs/tags/v4.0")
		if code := pushTag(t, "v4.1"); code != 1 {
			t.Fatalf("renamed push-tag exit %d, want 1", code)
		}
	})

	t.Run("refuses a tag that does not exist", func(t *testing.T) {
		if code := pushTag(t, "v9.9"); code != 1 {
			t.Fatalf("missing push-tag exit %d, want 1", code)
		}
	})

	t.Run("refuses a remote that is not allowlisted", func(t *testing.T) {
		setAllowedRemotes(t)
		gitRun(t, work, "tag", "v5.0")
		if code := pushTag(t, "v5.0"); code != 1 {
			t.Fatalf("unlisted push-tag exit %d, want 1", code)
		}
	})
}

func pushTag(t *testing.T, args ...string) int {
	t.Helper()
	var code int
	silenceStdio(t, func() { code = runGitSafePushTag(args) })
	return code
}

func remoteTag(t *testing.T, repo, tag string) string {
	t.Helper()
	out := strings.TrimSpace(gitRun(t, repo, "ls-remote", "origin", "refs/tags/"+tag))
	if out == "" {
		return ""
	}
	return strings.Fields(out)[0]
}
