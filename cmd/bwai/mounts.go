package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Tells whether name matches any direct (slash-free) pattern in the list
func matchesDirect(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if strings.Contains(pattern, "/") {
			continue
		}
		if matched, err := filepath.Match(pattern, name); err == nil && matched {
			return true
		}
	}
	return false
}

// Calls mount() for each sub-path entry in patterns that exists on disk
func subPathMounts(home string, patterns []string, mount func(string) []string) []string {
	var args []string
	for _, pattern := range patterns {
		if !strings.Contains(pattern, "/") {
			continue
		}
		p := filepath.Join(home, pattern)
		if _, err := os.Stat(p); err == nil {
			args = append(args, mount(p)...)
		}
	}
	return args
}

// Mount every dotfile and dotdir in $HOME, except those that are sensitive.
// Entries in homeAllowed are mounted as read-write. Everything else are
// mounted as read-only or hidden with tmpfs
func homeMounts(home string) []string {
	entries, err := os.ReadDir(home) // already sorted by name
	if err != nil {
		return nil
	}
	var args []string
	for _, entry := range entries {
		name := entry.Name()
		if matchesDirect(homeBlock, name) || name[0] != '.' {
			continue
		}
		p := filepath.Join(home, name)
		// Stat follows symlinks: bwrap resolves bind sources on the host,
		// so a dangling link (e.g. ~/.steampath) would abort the launch.
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if matchesDirect(homeAllow, name) {
			args = append(args, rwBind(p)...)
		} else {
			args = append(args, roBind(p)...)
		}
	}
	// Apply sub-path overrides after all parent dirs are mounted.
	// Blocked sub-paths must be hidden first, then allowed sub-paths can
	// selectively re-expose specific files within blocked directories.
	// Only directories get --tmpfs here; blocked *files* are masked by the
	// caller (blockedSubPathFiles), since --tmpfs can't mount over a file.
	args = append(args, subPathMounts(home, homeBlock, func(p string) []string {
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			return nil
		}
		return tmpfs(p)
	})...)
	args = append(args, subPathMounts(home, homeAllow, func(p string) []string { return rwBind(p) })...)
	return args
}

// blockedSubPathFiles lists homeBlock sub-path entries that exist as
// regular files. --tmpfs can only cover directories, so homeMounts leaves
// these for the caller to mask with an empty --file (e.g. a cargo or npm
// token file). An entry that home_allow also names is skipped, preserving
// the documented rule that home_allow wins for sub-paths.
func blockedSubPathFiles(home string, patterns, allowed []string) []string {
	allowedPaths := make(map[string]bool, len(allowed))
	for _, pattern := range allowed {
		if strings.Contains(pattern, "/") {
			allowedPaths[filepath.Join(home, pattern)] = true
		}
	}
	var files []string
	for _, pattern := range patterns {
		if !strings.Contains(pattern, "/") {
			continue
		}
		p := filepath.Join(home, pattern)
		if allowedPaths[p] {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			files = append(files, p)
		}
	}
	return files
}

// Bind /dev/shm (shared memory) if it exists on this host
func shmMount() []string {
	p := "/dev/shm"
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		return devBind(p)
	}
	return nil
}

// Restore /run/systemd/resolve after the --tmpfs /run overlay,
// otherwise the sandboxed agent won't be able to read resolv.conf and
// won't have network access
func dnsMounts() []string {
	p := "/run/systemd/resolve"
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		return roBind(p)
	}
	return nil
}

// etcResolvMount follows the sandbox's /etc/resolv.conf symlink and binds
// the host file at its real path so the link resolves inside the sandbox.
// Without it the link dangles (its target lives under /run/host, which the
// tmpfs /run overlay hides), so Go programs find no nameserver and every
// DNS lookup fails while glibc programs still resolve through nss-resolve.
func etcResolvMount() []string {
	return resolvSymlinkMount("/etc/resolv.conf", "/run/host")
}

func resolvSymlinkMount(etcResolv, hostRoot string) []string {
	dst, err := os.Readlink(etcResolv)
	if err != nil || !filepath.IsAbs(dst) || !isWithin(hostRoot, dst) {
		return nil
	}
	if _, err := os.Stat(dst); err != nil {
		return nil
	}
	return roBind(dst, dst)
}

// Bind GPU device nodes into the sandbox if they exist on this host.
// This includes the DRI subsystem and any NVIDIA character devices.
func gpuMounts() []string {
	var args []string
	if info, err := os.Stat("/dev/dri"); err == nil && info.IsDir() {
		args = append(args, devBind("/dev/dri")...)
	}
	matches, err := filepath.Glob("/dev/nvidia*")
	if err == nil {
		sort.Strings(matches)
		for _, p := range matches {
			args = append(args, devBind(p)...)
		}
	}
	return args
}

// readGitdirPointer reads a linked-worktree ".git" *file* and returns the
// absolute path it points at via its `gitdir: <path>` line. A relative
// <path> is resolved against the worktree root (the dir holding the .git
// file). Returns "" if the file isn't a gitdir pointer.
func readGitdirPointer(dotGitFile string) string {
	data, err := os.ReadFile(dotGitFile)
	if err != nil {
		return ""
	}
	const prefix = "gitdir:"
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(dotGitFile), p)
		}
		return filepath.Clean(p)
	}
	return ""
}

// resolveCommonDir returns the shared git common dir for a worktree gitdir.
// It reads <gitDir>/commondir (a relative path against gitDir); absent that
// file, gitDir is itself the common dir.
func resolveCommonDir(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	p := strings.TrimSpace(string(data))
	if p == "" {
		return gitDir
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(gitDir, p)
	}
	return filepath.Clean(p)
}

// isWithin reports whether child is parent itself or nested under it.
func isWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// worktreeRootMounts exposes a host directory for worktrees created inside
// the sandbox. A main checkout gets one dedicated sibling root; worktrunk's
// default sibling layout would otherwise land on the sandbox's tmpfs home
// and vanish with the session. Pointing WORKTRUNK_WORKTREE_PATH at the bound
// root (done by the caller) makes those worktrees persist.
//
// Only the dedicated root is exposed, not the repo's parent, so sibling
// checkouts stay hidden. Linked worktrees (.git is a file) are covered by
// gitWorktreeMounts instead, and non-git dirs get nothing.
//
// Returns the bind args and the host root, or nil/"" when currentDir is not
// a main git checkout.
func worktreeRootMounts(currentDir string) ([]string, string, error) {
	if !isMainCheckout(currentDir) {
		return nil, "", nil
	}
	root := filepath.Join(filepath.Dir(currentDir), "."+filepath.Base(currentDir)+".worktrees")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, "", err
	}
	return rwBind(root), root, nil
}

// gitWorktreeMounts exposes the shared git dir for a linked worktree so git
// can resolve its real repo from inside the sandbox. When currentDir is an
// ordinary checkout (.git is a directory) or not a git repo at all, it
// returns nil — the existing currentDir bind already covers those.
//
// The shared common dir holds objects, refs, AND the per-worktree gitdir
// nested under .git/worktrees/, so a single rw bind makes git behave exactly
// as in a normal checkout. The gitdir is only mounted separately in the rare
// case it lives outside the common dir (relocated/separate gitdir).
func gitWorktreeMounts(currentDir string) []string {
	dotGit := filepath.Join(currentDir, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil || info.IsDir() {
		return nil
	}
	gitDir := readGitdirPointer(dotGit)
	if gitDir == "" {
		return nil
	}
	commonDir := resolveCommonDir(gitDir)

	var args []string
	if _, err := os.Stat(commonDir); err == nil {
		args = append(args, rwBind(commonDir)...)
	}
	if !isWithin(commonDir, gitDir) {
		if _, err := os.Stat(gitDir); err == nil {
			args = append(args, rwBind(gitDir)...)
		}
	}
	return args
}
