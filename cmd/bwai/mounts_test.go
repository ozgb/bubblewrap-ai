package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchesDirect(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		input    string
		want     bool
	}{
		{"exact match", []string{".ssh"}, ".ssh", true},
		{"no match", []string{".ssh"}, ".gnupg", false},
		{"glob asterisk suffix", []string{".bash_history*"}, ".bash_history", true},
		{"glob asterisk matches extension", []string{".bash_history*"}, ".bash_history.bak", true},
		{"slash pattern is skipped", []string{".config/goose"}, ".config", false},
		{"slash pattern does not match leaf", []string{".config/goose"}, "goose", false},
		{"empty patterns", []string{}, ".ssh", false},
		{"first of multiple matches", []string{".ssh", ".gnupg"}, ".ssh", true},
		{"second of multiple matches", []string{".ssh", ".gnupg"}, ".gnupg", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesDirect(tt.patterns, tt.input)
			if got != tt.want {
				t.Errorf("matchesDirect(%v, %q) = %v, want %v", tt.patterns, tt.input, got, tt.want)
			}
		})
	}
}

func TestSubPathMounts(t *testing.T) {
	home := t.TempDir()

	existingDir := filepath.Join(home, ".config", "goose")
	if err := os.MkdirAll(existingDir, 0755); err != nil {
		t.Fatal(err)
	}

	patterns := []string{
		".ssh",            // direct (no slash) must be skipped
		".config/goose",   // sub-path, exists on disk
		".config/missing", // sub-path, does not exist
	}

	var mounted []string
	mount := func(p string) []string {
		mounted = append(mounted, p)
		return []string{"--test", p}
	}

	args := subPathMounts(home, patterns, mount)

	if len(mounted) != 1 || mounted[0] != existingDir {
		t.Errorf("expected mount called once with %q, got %v", existingDir, mounted)
	}
	if !containsSequence(args, "--test", existingDir) {
		t.Errorf("args missing expected sequence; got %v", args)
	}
}

func TestGPUMounts(t *testing.T) {
	// gpuMounts() reads from the real /dev, so we can only assert stable
	// structural properties: arguments come in --dev-bind triplets and each
	// source path belongs to the expected /dev/dri or /dev/nvidia* families.
	args := gpuMounts()

	for i := 0; i < len(args); i += 3 {
		if args[i] != "--dev-bind" {
			t.Errorf("expected --dev-bind at index %d, got %q", i, args[i])
		}
		if i+2 >= len(args) {
			t.Fatalf("incomplete --dev-bind triplet at index %d: %v", i, args[i:])
		}
		src := args[i+1]
		if _, err := os.Stat(src); err != nil {
			t.Errorf("gpuMounts returned non-existent path %q: %v", src, err)
		}
		if !strings.HasPrefix(src, "/dev/dri") && !strings.HasPrefix(src, "/dev/nvidia") {
			t.Errorf("gpuMounts returned unexpected path %q", src)
		}
	}
}

func TestHomeMounts(t *testing.T) {
	home := t.TempDir()

	// Mock package-level globals for the duration of the test.
	origAllowed := homeAllow
	origBlocked := homeBlock
	t.Cleanup(func() {
		homeAllow = origAllowed
		homeBlock = origBlocked
	})

	homeAllow = []string{".claude", ".config/goose"}
	homeBlock = []string{".ssh", ".config/secret"}

	mkDir := func(rel string) string {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mkFile := func(rel string) string {
		p := filepath.Join(home, rel)
		if err := os.WriteFile(p, nil, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	claudeDir := mkDir(".claude")        // allowed directory: --bind
	_ = mkDir(".ssh")                    // blocked directory: not mounted
	vimDir := mkDir(".vim")              // unclassified dotdir: --ro-bind
	_ = mkFile("README.md")              // non-dotfile: not mounted
	gooseDir := mkDir(".config/goose")   // allowed sub-path: --bind (last)
	secretDir := mkDir(".config/secret") // blocked sub-path: --tmpfs (before allowed)

	args := homeMounts(home)

	t.Run("allowed dotdir is rw-bound", func(t *testing.T) {
		if !containsSequence(args, "--bind", claudeDir, claudeDir) {
			t.Errorf("expected --bind for allowed dir %q; args: %v", claudeDir, args)
		}
	})

	t.Run("blocked dotdir is not mounted at parent level", func(t *testing.T) {
		sshDir := filepath.Join(home, ".ssh")
		if containsSequence(args, "--ro-bind", sshDir, sshDir) || containsSequence(args, "--bind", sshDir, sshDir) {
			t.Errorf("blocked dir %q must not appear as a parent-level mount; args: %v", sshDir, args)
		}
	})

	t.Run("unclassified dotdir is ro-bound", func(t *testing.T) {
		if !containsSequence(args, "--ro-bind", vimDir, vimDir) {
			t.Errorf("expected --ro-bind for unclassified dir %q; args: %v", vimDir, args)
		}
	})

	t.Run("non-dotfile is not mounted", func(t *testing.T) {
		readme := filepath.Join(home, "README.md")
		for _, a := range args {
			if a == readme {
				t.Errorf("non-dotfile %q must not appear in args; args: %v", readme, args)
			}
		}
	})

	t.Run("blocked sub-path gets tmpfs", func(t *testing.T) {
		if !containsSequence(args, "--tmpfs", secretDir) {
			t.Errorf("expected --tmpfs for blocked sub-path %q; args: %v", secretDir, args)
		}
	})

	t.Run("allowed sub-path gets rw-bind", func(t *testing.T) {
		if !containsSequence(args, "--bind", gooseDir, gooseDir) {
			t.Errorf("expected --bind for allowed sub-path %q; args: %v", gooseDir, args)
		}
	})

	t.Run("blocked sub-path tmpfs precedes allowed sub-path rw-bind", func(t *testing.T) {
		iBlocked := indexOfSequence(args, "--tmpfs", secretDir)
		iAllowed := indexOfSequence(args, "--bind", gooseDir, gooseDir)
		if iBlocked == -1 || iAllowed == -1 {
			t.Fatal("prerequisite sequences not found in args")
		}
		if iBlocked > iAllowed {
			t.Errorf("--tmpfs for blocked sub-path (idx %d) must come before --bind for allowed sub-path (idx %d)", iBlocked, iAllowed)
		}
	})
}

func TestReadGitdirPointer(t *testing.T) {
	t.Run("absolute pointer", func(t *testing.T) {
		dir := t.TempDir()
		dotGit := filepath.Join(dir, ".git")
		target := filepath.Join(t.TempDir(), "worktrees", "wt")
		if err := os.WriteFile(dotGit, []byte("gitdir: "+target+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if got := readGitdirPointer(dotGit); got != target {
			t.Errorf("readGitdirPointer = %q, want %q", got, target)
		}
	})

	t.Run("relative pointer resolves against worktree root", func(t *testing.T) {
		dir := t.TempDir()
		dotGit := filepath.Join(dir, ".git")
		if err := os.WriteFile(dotGit, []byte("gitdir: ../main/.git/worktrees/wt\n"), 0644); err != nil {
			t.Fatal(err)
		}
		want := filepath.Clean(filepath.Join(dir, "../main/.git/worktrees/wt"))
		if got := readGitdirPointer(dotGit); got != want {
			t.Errorf("readGitdirPointer = %q, want %q", got, want)
		}
	})

	t.Run("not a gitdir pointer", func(t *testing.T) {
		dir := t.TempDir()
		dotGit := filepath.Join(dir, ".git")
		if err := os.WriteFile(dotGit, []byte("something else\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if got := readGitdirPointer(dotGit); got != "" {
			t.Errorf("readGitdirPointer = %q, want empty", got)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if got := readGitdirPointer(filepath.Join(t.TempDir(), ".git")); got != "" {
			t.Errorf("readGitdirPointer = %q, want empty", got)
		}
	})
}

func TestResolveCommonDir(t *testing.T) {
	t.Run("relative commondir", func(t *testing.T) {
		gitDir := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "wt")
		if err := os.MkdirAll(gitDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0644); err != nil {
			t.Fatal(err)
		}
		want := filepath.Clean(filepath.Join(gitDir, "../.."))
		if got := resolveCommonDir(gitDir); got != want {
			t.Errorf("resolveCommonDir = %q, want %q", got, want)
		}
	})

	t.Run("no commondir file falls back to gitDir", func(t *testing.T) {
		gitDir := t.TempDir()
		if got := resolveCommonDir(gitDir); got != gitDir {
			t.Errorf("resolveCommonDir = %q, want %q", got, gitDir)
		}
	})
}

func TestIsWithin(t *testing.T) {
	tests := []struct {
		name          string
		parent, child string
		want          bool
	}{
		{"identical", "/a/b", "/a/b", true},
		{"nested", "/a/b", "/a/b/c", true},
		{"sibling", "/a/b", "/a/c", false},
		{"parent of", "/a/b", "/a", false},
		{"prefix but not nested", "/a/b", "/a/bc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWithin(tt.parent, tt.child); got != tt.want {
				t.Errorf("isWithin(%q, %q) = %v, want %v", tt.parent, tt.child, got, tt.want)
			}
		})
	}
}

func TestGitWorktreeMounts(t *testing.T) {
	t.Run("ordinary checkout returns nil", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
			t.Fatal(err)
		}
		if got := gitWorktreeMounts(dir); got != nil {
			t.Errorf("expected nil for ordinary checkout, got %v", got)
		}
	})

	t.Run("non-git dir returns nil", func(t *testing.T) {
		if got := gitWorktreeMounts(t.TempDir()); got != nil {
			t.Errorf("expected nil for non-git dir, got %v", got)
		}
	})

	t.Run("gitdir nested under common dir yields single bind", func(t *testing.T) {
		root := t.TempDir()
		// main repo .git with the worktree gitdir nested under it
		commonDir := filepath.Join(root, "main", ".git")
		gitDir := filepath.Join(commonDir, "worktrees", "wt")
		if err := os.MkdirAll(gitDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0644); err != nil {
			t.Fatal(err)
		}
		// the linked worktree itself
		wt := filepath.Join(root, "wt")
		if err := os.MkdirAll(wt, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0644); err != nil {
			t.Fatal(err)
		}

		args := gitWorktreeMounts(wt)
		want := filepath.Clean(commonDir)
		if !containsSequence(args, "--bind", want, want) {
			t.Errorf("expected single --bind of common dir %q; args: %v", want, args)
		}
		// only one --bind total
		count := 0
		for _, a := range args {
			if a == "--bind" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected exactly one --bind, got %d; args: %v", count, args)
		}
	})

	t.Run("gitdir outside common dir yields two binds", func(t *testing.T) {
		root := t.TempDir()
		commonDir := filepath.Join(root, "main", ".git")
		gitDir := filepath.Join(root, "elsewhere", "gitdir")
		if err := os.MkdirAll(commonDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(gitDir, 0755); err != nil {
			t.Fatal(err)
		}
		// commondir points outside gitDir, to the separate common dir
		if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte(commonDir+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		wt := filepath.Join(root, "wt")
		if err := os.MkdirAll(wt, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0644); err != nil {
			t.Fatal(err)
		}

		args := gitWorktreeMounts(wt)
		wantCommon := filepath.Clean(commonDir)
		wantGitDir := filepath.Clean(gitDir)
		if !containsSequence(args, "--bind", wantCommon, wantCommon) {
			t.Errorf("expected --bind of common dir %q; args: %v", wantCommon, args)
		}
		if !containsSequence(args, "--bind", wantGitDir, wantGitDir) {
			t.Errorf("expected --bind of separate gitdir %q; args: %v", wantGitDir, args)
		}
	})

	t.Run("relative pointer and relative commondir resolve correctly", func(t *testing.T) {
		root := t.TempDir()
		commonDir := filepath.Join(root, "main", ".git")
		gitDir := filepath.Join(commonDir, "worktrees", "wt")
		if err := os.MkdirAll(gitDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0644); err != nil {
			t.Fatal(err)
		}
		wt := filepath.Join(root, "wt")
		if err := os.MkdirAll(wt, 0755); err != nil {
			t.Fatal(err)
		}
		// relative gitdir pointer, relative to the worktree root
		rel, err := filepath.Rel(wt, gitDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+rel+"\n"), 0644); err != nil {
			t.Fatal(err)
		}

		args := gitWorktreeMounts(wt)
		want := filepath.Clean(commonDir)
		if !containsSequence(args, "--bind", want, want) {
			t.Errorf("expected --bind of common dir %q; args: %v", want, args)
		}
	})
}

// containsSequence reports whether needle appears as a contiguous subsequence in haystack.
func containsSequence(haystack []string, needle ...string) bool {
	return indexOfSequence(haystack, needle...) != -1
}

// indexOfSequence returns the index of the first element of the first occurrence of
// needle as a contiguous subsequence in haystack, or -1 if not found.
func indexOfSequence(haystack []string, needle ...string) int {
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j, v := range needle {
			if haystack[i+j] != v {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
