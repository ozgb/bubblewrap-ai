package main

import (
	"os"
	"path/filepath"
	"slices"
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
	homeBlock = []string{".ssh", ".config/secret", ".config/token"}

	mkDir := func(rel string) string {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mkFile := func(rel string) string {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	claudeDir := mkDir(".claude")         // allowed directory: --bind
	_ = mkDir(".ssh")                     // blocked directory: not mounted
	vimDir := mkDir(".vim")               // unclassified dotdir: --ro-bind
	_ = mkFile("README.md")               // non-dotfile: not mounted
	gooseDir := mkDir(".config/goose")    // allowed sub-path: --bind (last)
	secretDir := mkDir(".config/secret")  // blocked sub-path dir: --tmpfs (before allowed)
	secretFile := mkFile(".config/token") // blocked sub-path file: masked by caller, not --tmpfs

	dangling := filepath.Join(home, ".dangling")
	if err := os.Symlink(filepath.Join(home, ".missing-target"), dangling); err != nil {
		t.Fatal(err)
	}
	liveTarget := mkFile(".live-target")
	liveLink := filepath.Join(home, ".live-link")
	if err := os.Symlink(liveTarget, liveLink); err != nil {
		t.Fatal(err)
	}

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

	t.Run("blocked sub-path file is not tmpfs'd", func(t *testing.T) {
		if containsSequence(args, "--tmpfs", secretFile) {
			t.Errorf("blocked file %q must not get --tmpfs (bwrap rejects a file target); args: %v", secretFile, args)
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

	t.Run("dangling dotfile symlink is skipped", func(t *testing.T) {
		for _, a := range args {
			if a == dangling {
				t.Errorf("dangling symlink %q must not appear in args (bwrap cannot bind a missing target); args: %v", dangling, args)
			}
		}
	})

	t.Run("live dotfile symlink is ro-bound", func(t *testing.T) {
		if !containsSequence(args, "--ro-bind", liveLink, liveLink) {
			t.Errorf("expected --ro-bind for live symlink %q; args: %v", liveLink, args)
		}
	})
}

// TestBlockedSubPathFiles pins the split that makes file masking possible:
// only blocked sub-paths that are regular files are returned, since
// homeMounts handles directories and the caller masks files.
func TestBlockedSubPathFiles(t *testing.T) {
	home := t.TempDir()
	writeFile := func(rel string) string {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("token = \"x\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	file := writeFile(".cargo/credentials.toml")
	allowedFile := writeFile(".cargo/allowed.toml")
	dir := filepath.Join(home, ".config", "secret")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	got := blockedSubPathFiles(home, []string{
		".npmrc",                  // direct: handled as absent, not returned
		".cargo/credentials.toml", // file: returned
		".config/secret",          // directory: homeMounts tmpfs's it
		".cargo/missing",          // absent: skipped
		".cargo/allowed.toml",     // blocked but also allowed: allow wins
	}, []string{".cargo/allowed.toml"})
	if len(got) != 1 || got[0] != file {
		t.Errorf("blockedSubPathFiles = %v, want [%s] (allowedFile %s must be excluded)", got, file, allowedFile)
	}
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

func TestResolvSymlinkMount(t *testing.T) {
	t.Run("binds a symlink under the host root", func(t *testing.T) {
		base := t.TempDir()
		hostRoot := filepath.Join(base, "run/host")
		dir := filepath.Join(hostRoot, "etc")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		conf := filepath.Join(dir, "resolv.conf")
		if err := os.WriteFile(conf, []byte("nameserver 127.0.0.53\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		etcResolv := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.Symlink(conf, etcResolv); err != nil {
			t.Fatal(err)
		}
		want := roBind(conf, conf)
		if got := resolvSymlinkMount(etcResolv, hostRoot); !slices.Equal(got, want) {
			t.Errorf("resolvSymlinkMount = %v, want %v", got, want)
		}
	})
	t.Run("skips a symlink outside the host root", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "real.conf")
		if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		etcResolv := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.Symlink(other, etcResolv); err != nil {
			t.Fatal(err)
		}
		if got := resolvSymlinkMount(etcResolv, filepath.Join(t.TempDir(), "run/host")); got != nil {
			t.Errorf("resolvSymlinkMount = %v, want nil", got)
		}
	})
	t.Run("skips a real file", func(t *testing.T) {
		etcResolv := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.WriteFile(etcResolv, []byte("nameserver 127.0.0.53\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := resolvSymlinkMount(etcResolv, filepath.Join(t.TempDir(), "run/host")); got != nil {
			t.Errorf("resolvSymlinkMount = %v, want nil", got)
		}
	})
	t.Run("skips a dangling symlink", func(t *testing.T) {
		etcResolv := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing.conf"), etcResolv); err != nil {
			t.Fatal(err)
		}
		if got := resolvSymlinkMount(etcResolv, filepath.Join(t.TempDir(), "run/host")); got != nil {
			t.Errorf("resolvSymlinkMount = %v, want nil", got)
		}
	})
}

func TestWorktreeRootMounts(t *testing.T) {
	t.Run("main checkout gets a dedicated sibling root", func(t *testing.T) {
		base := t.TempDir()
		repo := filepath.Join(base, "myrepo")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644); err != nil {
			t.Fatal(err)
		}

		args, root, err := worktreeRootMounts(repo)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(base, ".myrepo.worktrees")
		if root != want {
			t.Errorf("root = %q, want %q", root, want)
		}
		if !containsSequence(args, "--bind", want, want) {
			t.Errorf("expected --bind of worktree root %q; args: %v", want, args)
		}
		if info, err := os.Stat(want); err != nil || !info.IsDir() {
			t.Errorf("worktree root not created on disk: err=%v", err)
		}
	})

	t.Run("non-git dir yields nothing", func(t *testing.T) {
		args, root, err := worktreeRootMounts(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if args != nil || root != "" {
			t.Errorf("expected no mounts, got args=%v root=%q", args, root)
		}
	})

	t.Run("linked worktree yields nothing", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /somewhere\n"), 0644); err != nil {
			t.Fatal(err)
		}
		args, root, err := worktreeRootMounts(dir)
		if err != nil {
			t.Fatal(err)
		}
		if args != nil || root != "" {
			t.Errorf("expected no mounts, got args=%v root=%q", args, root)
		}
	})

	t.Run(".git without HEAD is not a repo", func(t *testing.T) {
		repo := filepath.Join(t.TempDir(), "notrepo")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
			t.Fatal(err)
		}
		args, root, err := worktreeRootMounts(repo)
		if err != nil {
			t.Fatal(err)
		}
		if args != nil || root != "" {
			t.Errorf("expected no mounts, got args=%v root=%q", args, root)
		}
	})
}

// makeLinkedWorktree builds a main checkout plus one linked worktree and
// returns both paths. The layout mirrors real git: the worktree's .git
// file points at a gitdir nested under the main .git, whose commondir
// climbs back to the main .git.
func makeLinkedWorktree(t *testing.T) (main, wt string) {
	t.Helper()
	base := t.TempDir()
	main = filepath.Join(base, "main")
	wt = filepath.Join(base, "wt")
	gitDir := filepath.Join(main, ".git", "worktrees", "wt")
	for _, d := range []string{main, wt, gitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(main, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "gitdir"), []byte(wt+"/.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return main, wt
}

func TestWorktreeSiblingMounts(t *testing.T) {
	t.Run("expose_main binds main tree and managed root", func(t *testing.T) {
		main, wt := makeLinkedWorktree(t)
		args, root, mainTree, err := worktreeSiblingMounts(wt, true)
		if err != nil {
			t.Fatal(err)
		}
		wantRoot := filepath.Join(filepath.Dir(main), ".main.worktrees")
		if root != wantRoot || mainTree != main {
			t.Errorf("root=%q mainTree=%q, want %q/%q", root, mainTree, wantRoot, main)
		}
		if !containsSequence(args, "--bind", main, main) {
			t.Errorf("expected --bind of main tree %q; args: %v", main, args)
		}
		if !containsSequence(args, "--bind", wantRoot, wantRoot) {
			t.Errorf("expected --bind of managed root %q; args: %v", wantRoot, args)
		}
		if info, err := os.Stat(wantRoot); err != nil || !info.IsDir() {
			t.Errorf("managed root not created on disk: err=%v", err)
		}
	})

	t.Run("expose_main=false binds only the managed root", func(t *testing.T) {
		main, wt := makeLinkedWorktree(t)
		args, _, mainTree, err := worktreeSiblingMounts(wt, false)
		if err != nil {
			t.Fatal(err)
		}
		if mainTree != main || !strings.Contains(strings.Join(args, " "), ".main.worktrees") {
			t.Fatalf("expected managed root bind, got args=%v mainTree=%q", args, mainTree)
		}
		if containsSequence(args, "--bind", main, main) {
			t.Errorf("main tree %q must not be bound when expose_main is off; args: %v", main, args)
		}
	})

	t.Run("main checkout yields nothing", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		args, root, mainTree, err := worktreeSiblingMounts(dir, true)
		if err != nil {
			t.Fatal(err)
		}
		if args != nil || root != "" || mainTree != "" {
			t.Errorf("expected nothing, got args=%v root=%q mainTree=%q", args, root, mainTree)
		}
	})

	t.Run("non-git dir yields nothing", func(t *testing.T) {
		args, root, mainTree, err := worktreeSiblingMounts(t.TempDir(), true)
		if err != nil {
			t.Fatal(err)
		}
		if args != nil || root != "" || mainTree != "" {
			t.Errorf("expected nothing, got args=%v root=%q mainTree=%q", args, root, mainTree)
		}
	})

	t.Run("worktree-of-worktree yields nothing", func(t *testing.T) {
		// The "main" tree itself has a .git *file*, so no main checkout can
		// be derived and the managed-root name would not match the one a
		// main-checkout session computes.
		base := t.TempDir()
		outer := filepath.Join(base, "outer")
		inner := filepath.Join(base, "inner")
		for _, d := range []string{outer, inner} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		gitDir := filepath.Join(base, "shared", "gitdir")
		if err := os.MkdirAll(gitDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte(".\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outer, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inner, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		args, root, mainTree, err := worktreeSiblingMounts(inner, true)
		if err != nil {
			t.Fatal(err)
		}
		if args != nil || root != "" || mainTree != "" {
			t.Errorf("expected nothing, got args=%v root=%q mainTree=%q", args, root, mainTree)
		}
	})
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
