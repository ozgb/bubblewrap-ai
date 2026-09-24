package main

import (
	"os"
	"path/filepath"
)

// worktreeSiblingMounts covers the case that worktreeRootMounts cannot:
// bwai started inside a linked worktree rather than the main checkout.
// The managed worktree root is derived from the main tree — anchored at
// the repo, not the current worktree — so every session of a repo,
// wherever it starts, shares one persistent root and one WORKTRUNK_WORKTREE_PATH.
//
// Only the managed root is exposed, matching the main-checkout policy
// that sibling checkouts stay hidden. The main checkout's working tree
// is mounted only when exposeMain is set. The shared git dir is already
// covered by gitWorktreeMounts.
//
// Returns the bind args, the host root, and the main tree path (the
// bind source for expose_main, useful to the broker's cwd allowlist),
// or nil/""/"" when currentDir is not a linked worktree.
func worktreeSiblingMounts(currentDir string, exposeMain bool) ([]string, string, string, error) {
	dotGit := filepath.Join(currentDir, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil || info.IsDir() {
		return nil, "", "", nil
	}
	gitDir := readGitdirPointer(dotGit)
	if gitDir == "" {
		return nil, "", "", nil
	}
	commonDir := resolveCommonDir(gitDir)
	mainTree := filepath.Dir(commonDir)
	// The main tree must itself be a main checkout (.git a directory with
	// HEAD). A .git file there would mean a worktree-of-worktree layout,
	// where the derived root would not match what main-checkout sessions
	// compute — refuse rather than fork the namespace.
	if !isMainCheckout(mainTree) {
		return nil, "", "", nil
	}
	root := filepath.Join(filepath.Dir(mainTree), "."+filepath.Base(mainTree)+".worktrees")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, "", "", err
	}
	var args []string
	if exposeMain && mainTree != currentDir {
		args = append(args, rwBind(mainTree)...)
	}
	args = append(args, rwBind(root)...)
	return args, root, mainTree, nil
}

// isMainCheckout reports whether dir is a main checkout: .git is a
// directory holding HEAD. Worktrees (.git is a file) and non-repos are
// both false.
func isMainCheckout(dir string) bool {
	dotGit := filepath.Join(dir, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = os.Stat(filepath.Join(dotGit, "HEAD"))
	return err == nil
}
