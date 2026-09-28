package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
)

// gitSafeUsage is printed by `git-safe --help` and on argv errors.
const gitSafeUsage = `git-safe — a deliberately narrow git wrapper for the bwai broker.

Usage:
  git-safe push [<remote>]  Push the current branch to <remote> (default
                            origin), fast-forward only.

git-safe push accepts at most one remote name — no flags, no refspecs, no
URL. The remote is trusted only as far as its push URL appears in the
broker's push_allowed_urls: the sandbox owns .git/config and can retarget
any remote, so the URL, not the name, is what gets authorised. It refuses
a detached HEAD and the protected branches — the built-in main, master,
trunk, develop plus any patterns in broker.protected_branches — and never
performs a non-fast-forward update: the remote's tip must be an ancestor
of HEAD. The remote branch is created if it does not exist yet.

To commit, run git commit directly: it is signed with the host's key
through the broker (git's gpg.program is bwai-gpg inside the sandbox).

The policy lives in code rather than in an allowlist pattern. The broker's
matcher cannot express "any force flag, in any position", so commands that
need that judgement live behind a wrapper and the broker is left to match
a short argv: ["git-safe", "push", "**"].`

// gitSafeCommitRemoved answers the retired `git-safe commit`, which ran
// git commit on the host inside the agent's repository — and with it the
// repository's hooks and config.
const gitSafeCommitRemoved = "git-safe: `git-safe commit` has been removed; run `git commit` directly — it is signed through the broker automatically"

// defaultProtectedBranches are always refused, independent of config, so a
// config mistake cannot open the canonical branches. Configured patterns
// (broker.protected_branches) are added on top of these.
var defaultProtectedBranches = []string{"main", "master", "trunk", "develop"}

// runGitSafeClient is the sandbox-side half of git-safe, reached when
// the binary is invoked as `git-safe` from inside the sandbox. The
// policy lives on the host — the agent can reach everything the sandbox
// can reach, so a check performed in here would be advisory only — so
// this is a broker client, exactly like bwai-outside, with "git-safe"
// pinned to the front so the request matches the broker's
// ["git-safe", …] rule.
//
// --help is answered locally: it is pure text, and spending a broker
// round trip (or an approval) to print a usage string would be silly.
func runGitSafeClient(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "git-safe: missing subcommand")
		fmt.Fprintln(os.Stderr, gitSafeUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Println(gitSafeUsage)
		return 0
	case "commit":
		fmt.Fprintln(os.Stderr, gitSafeCommitRemoved)
		return 2
	case "push":
		branch, sha := sandboxHead()
		code := runOutsideExec(append([]string{"git-safe"}, args...))
		if code == 0 && branch != "" {
			recordPush(args[1:], branch, sha)
		}
		return code
	}
	return runOutsideExec(append([]string{"git-safe"}, args...))
}

// sandboxHead returns the current branch and its commit, as the sandbox's
// own git sees them. Empty when HEAD is detached or git fails.
func sandboxHead() (branch, sha string) {
	b, err := exec.Command("git", "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return "", ""
	}
	branch = strings.TrimSpace(string(b))
	s, err := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+branch).Output()
	if err != nil {
		return "", ""
	}
	return branch, strings.TrimSpace(string(s))
}

// recordPush updates the sandbox repository after a successful push: the
// host pushes from its own mirror and never writes to the agent's repo,
// so the remote-tracking ref and upstream are set here, where running git
// is the agent's own business. Best-effort: the push has already happened.
func recordPush(args []string, branch, sha string) {
	remote := "origin"
	if len(args) == 1 {
		remote = args[0]
	}
	tracking := "refs/remotes/" + remote + "/" + branch
	if exec.Command("git", "update-ref", tracking, sha).Run() != nil {
		return
	}
	if exec.Command("git", "rev-parse", "--verify", "--quiet", branch+"@{upstream}").Run() != nil {
		_ = exec.Command("git", "branch", "--quiet", "--set-upstream-to="+remote+"/"+branch, branch).Run()
	}
}

// runGitSafe is the host-side half: the `bwai git-safe …` subcommand,
// which the broker runs on the host once a ["git-safe", …] rule matches.
// Inside the sandbox the same name dispatches to runGitSafeClient above,
// so the two halves never share an entry point.
func runGitSafe(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "git-safe: missing subcommand")
		fmt.Fprintln(os.Stderr, gitSafeUsage)
		return 2
	}
	switch args[0] {
	case "push":
		return runGitSafePush(args[1:])
	case "commit":
		fmt.Fprintln(os.Stderr, gitSafeCommitRemoved)
		return 2
	case "-h", "--help", "help":
		fmt.Println(gitSafeUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "git-safe: unknown subcommand %q\n", args[0])
		fmt.Fprintln(os.Stderr, gitSafeUsage)
		return 2
	}
}

// runGitSafePush implements `git-safe push`. It never runs git in the
// agent's repository, which the agent controls down to its hooks and
// config. Instead it reads the few facts it needs from the git dir as
// plain files, pulls the branch into a host-owned mirror (upload-pack is
// the one git command designed to be safe against an untrusted
// repository), and pushes from the mirror to the allowlisted URL. The
// policy decision itself is planPush, which is pure and unit-tested.
func runGitSafePush(args []string) int {
	remote, err := planPushRemote(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 2
	}
	cwd := os.Getenv(requestCwdEnv)
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
			return 1
		}
	}
	repo, err := locateRepo(cwd, allowedRoots())
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: refusing to push: %v\n", err)
		return 1
	}
	branch, err := repo.branch()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: refusing to push: %v\n", err)
		return 1
	}

	protected, err := protectedBranchPatterns()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 1
	}
	if branchProtected(branch, protected) {
		fmt.Fprintf(os.Stderr, "git-safe: refusing to push protected branch %q\n", branch)
		return 1
	}

	// The sandbox owns .git/config, so a remote name proves nothing: the
	// push URL it resolves to only selects an allowlist entry, and the
	// entry's own spelling is what gets pushed to.
	rawURL, err := repo.pushURL(remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: refusing to push: %v\n", err)
		return 1
	}
	allowed, err := pushAllowedURLs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 1
	}
	dest := allowedEntry(normalizeRemoteURL(rawURL), allowed)
	if dest == "" {
		fmt.Fprintf(os.Stderr, "git-safe: refusing to push: remote %q (%s) is not in push_allowed_urls\n", remote, rawURL)
		return 1
	}

	mirror, err := ensureMirror(dest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: mirror: %v\n", err)
		return 1
	}
	incoming := fmt.Sprintf("refs/bwai/incoming/%d", os.Getpid())
	tracking := fmt.Sprintf("refs/bwai/remote/%d", os.Getpid())
	defer func() {
		_, _ = mirrorGit(mirror, "update-ref", "-d", incoming)
		_, _ = mirrorGit(mirror, "update-ref", "-d", tracking)
	}()
	// safe.directory is scoped to this fetch: it lets upload-pack serve a
	// repository owned by another user (the agent box), and upload-pack
	// reads that repository without executing anything from it.
	// --update-shallow accepts a branch from a shallow clone; without it
	// git drops the ref with only a warning and still exits 0.
	out, err := mirrorGit(mirror,
		"-c", "safe.directory="+repo.commonDir,
		"-c", "safe.directory="+repo.workTree,
		"-c", "transfer.fsckObjects=true",
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--update-shallow",
		repo.commonDir, "+refs/heads/"+branch+":"+incoming)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: cannot read branch %q from the repository: %v\n%s", branch, err, out)
		return 1
	}
	sha, err := mirrorGit(mirror, "rev-parse", "--verify", "--quiet", incoming)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: branch %q did not arrive in the host mirror\n%s", branch, out)
		return 1
	}
	sha = strings.TrimSpace(sha)

	remoteExists, fastForward := false, false
	if _, err := mirrorGit(mirror, "ls-remote", "--exit-code", dest, "refs/heads/"+branch); err == nil {
		remoteExists = true
		if out, ferr := mirrorGit(mirror, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
			dest, "+refs/heads/"+branch+":"+tracking); ferr != nil {
			fmt.Fprintf(os.Stderr, "git-safe: cannot fetch %s/%s: %v\n%s", remote, branch, ferr, out)
			return 1
		}
		if _, aerr := mirrorGit(mirror, "merge-base", "--is-ancestor", tracking, sha); aerr == nil {
			fastForward = true
		}
	}

	argv, err := planPush(branch, remote, dest, sha, remoteExists, fastForward, protected)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 1
	}
	if code := execGit(mirror, argv); code != 0 {
		return code
	}
	fmt.Printf("git-safe: pushed %s to %s/%s\n", sha[:12], remote, branch)
	return 0
}

// remoteNameRe matches the remote names git accepts. Anchored on an
// alphanumeric first character so a flag ("--force"), a URL, or a refspec
// can never reach git in the remote slot.
var remoteNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// planPushRemote is pure: it accepts zero or one remote name, defaulting to
// origin, and rejects anything else. Keeping the destination a bare remote
// name is what lets the URL check in runGitSafePush be the only way a
// remote is chosen.
func planPushRemote(args []string) (string, error) {
	switch len(args) {
	case 0:
		return "origin", nil
	case 1:
		if !remoteNameRe.MatchString(args[0]) {
			return "", fmt.Errorf("refusing to push: %q is not a valid remote name", args[0])
		}
		return args[0], nil
	default:
		return "", fmt.Errorf("refusing to push: at most one remote name is accepted")
	}
}

// planPush is the entire policy, kept pure so it can be tested without a
// repository. It returns the exact argv to run in the mirror, or an error
// explaining the refusal.
//
//	remote       — the agent's remote name, for messages only
//	dest         — the allowlist entry the remote's push URL matched
//	sha          — the commit being published, already in the mirror
//	remoteExists — dest already has refs/heads/<branch>
//	fastForward  — dest's tip is an ancestor of sha (only consulted when
//	               remoteExists is true)
//	protected    — branch names/globs that may never be pushed
func planPush(branch, remote, dest, sha string, remoteExists, fastForward bool, protected []string) ([]string, error) {
	if branch == "" {
		return nil, fmt.Errorf("refusing to push: no branch is checked out")
	}
	if branchProtected(branch, protected) {
		return nil, fmt.Errorf("refusing to push protected branch %q", branch)
	}
	if remoteExists && !fastForward {
		return nil, fmt.Errorf("refusing to push %q: %s/%s is not an ancestor of HEAD (fetch and merge or rebase first)", branch, remote, branch)
	}
	// The refspec is constructed here: no "+" prefix, no "--force", and no
	// way for the caller to name a different source or destination.
	// Combined with the ancestry check above, a non-fast-forward push
	// cannot happen.
	return []string{"git", "push", dest, sha + ":refs/heads/" + branch}, nil
}

// pushAllowedEnv carries the broker's push-allowlist snapshot to the
// host-side push. It is the snapshot, not an on-disk config read, that
// authorises: the project tree is writable inside the sandbox, so opening
// .bwai.json at push time could be widened mid-session.
const pushAllowedEnv = "BWAI_PUSH_ALLOWED"

// pushAllowedURLs returns the allowlist. The broker always sets the env var
// (empty when nothing is allowed); its presence is authoritative, so an
// in-sandbox invocation can never fall through to a file. When the var is
// absent — git-safe run by hand on the host — only the host-global config is
// consulted, never the project-local .bwai.json in the sandbox-writable tree.
func pushAllowedURLs() ([]string, error) {
	if v, ok := os.LookupEnv(pushAllowedEnv); ok {
		if v == "" {
			return nil, nil
		}
		return strings.Split(v, "\n"), nil
	}
	cfg, err := loadConfig(defaultConfigPath())
	if err != nil {
		return nil, err
	}
	return cfg.Broker.PushAllowedURLs, nil
}

// normalizeRemoteURL reduces the spellings git accepts to a canonical
// host/path, so the same repository matches the allowlist whether it is
// written scp-, ssh-, or https-style. User, port, a trailing slash, and a
// trailing .git are dropped: none of them changes which repository a push
// reaches.
func normalizeRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	var host, path string
	switch {
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil {
			return s
		}
		host, path = u.Hostname(), strings.TrimPrefix(u.Path, "/")
	case strings.Contains(s, ":"): // scp syntax: [user@]host:path
		i := strings.IndexByte(s, ':')
		host, path = s[:i], s[i+1:]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
	default:
		return s
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	return strings.ToLower(host) + "/" + path
}

// allowedEntry returns the allowlist entry that normalises to norm, or "".
// Each entry is normalised the same way, so the two sides need not be
// spelled alike.
func allowedEntry(norm string, allowed []string) string {
	for _, a := range allowed {
		if normalizeRemoteURL(a) == norm {
			return a
		}
	}
	return ""
}

// protectedBranchesEnv carries the broker's configured protected-branch
// patterns, snapshotted alongside pushAllowedEnv and for the same reason.
const protectedBranchesEnv = "BWAI_PROTECTED_BRANCHES"

// protectedBranchPatterns returns the built-in names plus any configured
// patterns. The env var is authoritative when present (the broker always
// sets it), so an in-sandbox invocation can never fall through to a file;
// when absent — run by hand on the host — only the host-global config is
// consulted, never the project-local .bwai.json.
func protectedBranchPatterns() ([]string, error) {
	pats := append([]string(nil), defaultProtectedBranches...)
	var extra []string
	if v, ok := os.LookupEnv(protectedBranchesEnv); ok {
		if v != "" {
			extra = strings.Split(v, "\n")
		}
	} else {
		cfg, err := loadConfig(defaultConfigPath())
		if err != nil {
			return nil, err
		}
		extra = cfg.Broker.ProtectedBranches
	}
	return append(pats, extra...), nil
}

// branchProtected reports whether branch matches any pattern. A pattern with
// no metacharacters is an exact match; otherwise it is a shell glob matched
// with path.Match, whose "*" stops at "/" — so "release/*" covers
// release/1.0 but not release/1.0/x, and "release-*" covers release-1.0.
func branchProtected(branch string, patterns []string) bool {
	for _, p := range patterns {
		if p == branch {
			return true
		}
		if ok, err := path.Match(p, branch); err == nil && ok {
			return true
		}
	}
	return false
}

// mirrorGit runs git in the host-owned mirror and returns its combined
// output.
func mirrorGit(mirror string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = mirror
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// execGit runs argv in dir with the caller's stdio, streaming output as a
// normal shell would, and propagates the exit code.
func execGit(dir string, argv []string) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 1
	}
	return 0
}
