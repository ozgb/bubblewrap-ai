package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// requestCwdEnv carries the sandbox's request cwd to host-side commands
// that need it. The broker runs every host command from an empty
// directory, so the agent's tree is never git's cwd; a wrapper that has to
// find the agent's repository reads this instead.
const requestCwdEnv = "BWAI_REQUEST_CWD"

// allowedRootsEnv carries the broker's writable roots (newline separated)
// so a wrapper can refuse a repository whose git dir lies outside them.
// Without it an agent could point .git at another of the host user's
// repositories and have the host read that one.
const allowedRootsEnv = "BWAI_ALLOWED_ROOTS"

// allowedRoots returns the broker's roots, or nil when unset (git-safe run
// by hand on the host, where the caller is trusted).
func allowedRoots() []string {
	v := os.Getenv(allowedRootsEnv)
	if v == "" {
		return nil
	}
	return strings.Split(v, "\n")
}

// hostRepo is an agent repository located without running git in it.
type hostRepo struct {
	workTree  string
	gitDir    string // per-worktree git dir (HEAD lives here)
	commonDir string // shared git dir (config, objects, refs)
}

// locateRepo walks up from cwd to the enclosing .git, following a linked
// worktree's gitdir pointer and commondir by reading them as files. Every
// resolved directory must lie within roots when roots is non-nil.
func locateRepo(cwd string, roots []string) (*hostRepo, error) {
	dir, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, err
	}
	for {
		dotGit := filepath.Join(dir, ".git")
		info, err := os.Lstat(dotGit)
		if err == nil {
			r := &hostRepo{workTree: dir}
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				return nil, fmt.Errorf("%s is a symlink", dotGit)
			case info.IsDir():
				r.gitDir = dotGit
			default:
				if r.gitDir = readGitdirPointer(dotGit); r.gitDir == "" {
					return nil, fmt.Errorf("%s is not a gitdir pointer", dotGit)
				}
			}
			r.commonDir = resolveCommonDir(r.gitDir)
			return r, r.confine(roots)
		}
		parent := filepath.Dir(dir)
		if parent == dir || (roots != nil && !withinAny(parent, roots)) {
			return nil, errors.New("not inside a git repository")
		}
		dir = parent
	}
}

// confine resolves the git dirs through symlinks and checks them against
// roots, and refuses object alternates, which would let the repository
// serve objects out of a store the agent cannot write.
func (r *hostRepo) confine(roots []string) error {
	for _, p := range []*string{&r.gitDir, &r.commonDir} {
		resolved, err := filepath.EvalSymlinks(*p)
		if err != nil {
			return err
		}
		*p = resolved
		if roots != nil && !withinAny(resolved, roots) {
			return fmt.Errorf("git dir %s is outside the sandbox's writable roots", resolved)
		}
	}
	for _, alt := range []string{"alternates", "http-alternates"} {
		if _, err := os.Lstat(filepath.Join(r.commonDir, "objects", "info", alt)); err == nil {
			return errors.New("repositories with object alternates are not supported")
		}
	}
	return nil
}

// branch reads the checked-out branch from HEAD.
func (r *hostRepo) branch() (string, error) {
	data, err := os.ReadFile(filepath.Join(r.gitDir, "HEAD"))
	if err != nil {
		return "", err
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "ref: refs/heads/")
	if !ok || ref == "" {
		return "", errors.New("HEAD is detached (check out a branch first)")
	}
	if ref == ".invalid" {
		return "", errors.New("reftable repositories are not supported")
	}
	return ref, nil
}

// pushURL reads remote.<name>.pushurl, falling back to .url, straight from
// the config file: `git config --file` parses that one file and nothing
// else (includes are off for --file), and runs from / so no repository is
// discovered around it.
func (r *hostRepo) pushURL(remote string) (string, error) {
	cfg := filepath.Join(r.commonDir, "config")
	for _, key := range []string{"pushurl", "url"} {
		cmd := exec.Command("git", "config", "--file", cfg, "--get-all", "remote."+remote+"."+key)
		cmd.Dir = "/"
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		urls := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(urls) != 1 {
			return "", fmt.Errorf("remote %q has %d push URLs; exactly one is required", remote, len(urls))
		}
		return urls[0], nil
	}
	return "", fmt.Errorf("no git remote named %q", remote)
}

func withinAny(path string, roots []string) bool {
	for _, root := range roots {
		if root != "" && isWithin(root, path) {
			return true
		}
	}
	return false
}

// brokerPrivateDir holds host-only broker state — the push mirrors. The
// sandbox gets a tmpfs over it: a mirror's hooks and config run with the
// host's credentials, so the agent must not be able to write them.
func brokerPrivateDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "bwai-broker")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "bwai-broker")
	}
	return filepath.Join(home, ".local", "share", "bwai-broker")
}

// ensureMirror returns the bare mirror for dest, creating it on first use.
// One mirror per destination URL, so repeated pushes fetch incrementally.
func ensureMirror(dest string) (string, error) {
	sum := sha256.Sum256([]byte(normalizeRemoteURL(dest)))
	dir := filepath.Join(brokerPrivateDir(), "mirrors", hex.EncodeToString(sum[:8])+".git")
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	if out, err := exec.Command("git", "init", "--quiet", "--bare", dir).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git init: %v: %s", err, out)
	}
	return dir, nil
}
