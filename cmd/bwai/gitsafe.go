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
  git-safe push [<remote>]          Push the current branch to <remote> (default
                                    origin), fast-forward only.
  git-safe commit -m <message> ...  Commit staged changes, GPG-signed.

git-safe push accepts at most one remote name — no flags, no refspecs, no
URL. The remote is trusted only as far as its push URL appears in the
broker's push_allowed_urls: the sandbox owns .git/config and can retarget
any remote, so the URL, not the name, is what gets authorised. It refuses
a detached HEAD and the protected branches — the built-in main, master,
trunk, develop plus any patterns in broker.protected_branches — and never
performs a non-fast-forward update: the remote's tip must be an ancestor
of HEAD. The remote branch is created if it does not exist yet.

git-safe commit takes only -m <message>, repeated for extra paragraphs,
and always signs with -S — the host's keyring is the reason to route a
commit through the broker in the first place. Every other git commit
spelling is refused: --amend, -a/--all, --no-verify, --author, -F, and
bare pathspecs.

The policy lives in code rather than in an allowlist pattern. The broker's
matcher cannot express "any force flag, in any position", so commands that
need that judgement live behind a wrapper and the broker is left to match
a short argv: ["git-safe", "push", "**"] or ["git-safe", "commit", "**"].`

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
	}
	return runOutsideExec(append([]string{"git-safe"}, args...))
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
		return runGitSafeCommit(args[1:])
	case "-h", "--help", "help":
		fmt.Println(gitSafeUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "git-safe: unknown subcommand %q\n", args[0])
		fmt.Fprintln(os.Stderr, gitSafeUsage)
		return 2
	}
}

// runGitSafePush implements `git-safe push`. It gathers the facts the
// policy needs from the repository, then defers the decision to planPush,
// which is pure and unit-tested.
func runGitSafePush(args []string) int {
	remote, err := planPushRemote(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 2
	}

	branch, err := gitOutput("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-safe: refusing to push: HEAD is detached (check out a branch first)")
		return 1
	}
	branch = strings.TrimSpace(branch)

	protected, err := protectedBranchPatterns()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 1
	}

	// The sandbox owns .git/config, so a remote name proves nothing: the
	// push URL it resolves to is what gets authorised, against the
	// allowlist the broker snapshotted at session start. --push so a
	// remote.<name>.pushurl is the URL that is checked.
	rawURL, err := gitOutput("remote", "get-url", "--push", remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: refusing to push: no git remote named %q\n", remote)
		return 1
	}
	allowed, err := pushAllowedURLs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 1
	}
	if !urlAllowed(normalizeRemoteURL(rawURL), allowed) {
		fmt.Fprintf(os.Stderr, "git-safe: refusing to push: remote %q (%s) is not in push_allowed_urls\n",
			remote, strings.TrimSpace(rawURL))
		return 1
	}

	// Does the branch already exist on the remote? `ls-remote --exit-code`
	// exits non-zero when nothing matched, so a nil error means it does.
	remoteExists := false
	fastForward := false
	if _, err := gitOutput("ls-remote", "--exit-code", remote, "refs/heads/"+branch); err == nil {
		remoteExists = true
		// Bring the remote tip local so ancestry is decidable, then
		// require it to be an ancestor of HEAD. This is what makes a
		// destructive update impossible rather than merely discouraged.
		if _, ferr := gitOutput("fetch", "--quiet", remote, "refs/heads/"+branch); ferr != nil {
			fmt.Fprintf(os.Stderr, "git-safe: cannot fetch %s/%s: %v\n", remote, branch, ferr)
			return 1
		}
		if _, aerr := gitOutput("merge-base", "--is-ancestor", "FETCH_HEAD", "HEAD"); aerr == nil {
			fastForward = true
		}
	}

	hasUpstream := false
	if _, err := gitOutput("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil {
		hasUpstream = true
	}

	argv, err := planPush(branch, remote, remoteExists, fastForward, hasUpstream, protected)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 1
	}
	return execGit(argv)
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
// repository. It returns the exact argv to run, or an error explaining the
// refusal.
//
//	remote       — the remote to push to, whose URL the caller has already
//	               authorised against push_allowed_urls
//	remoteExists — the remote already has refs/heads/<branch>
//	fastForward  — the remote's tip is an ancestor of HEAD (only consulted
//	               when remoteExists is true)
//	hasUpstream  — the local branch already tracks a remote branch
//	protected    — branch names/globs that may never be pushed
func planPush(branch, remote string, remoteExists, fastForward, hasUpstream bool, protected []string) ([]string, error) {
	if branch == "" {
		return nil, fmt.Errorf("refusing to push: no branch is checked out")
	}
	if branchProtected(branch, protected) {
		return nil, fmt.Errorf("refusing to push protected branch %q", branch)
	}
	if remoteExists && !fastForward {
		return nil, fmt.Errorf("refusing to push %q: %s/%s is not an ancestor of HEAD (fetch and merge or rebase first)", branch, remote, branch)
	}

	argv := []string{"git", "push"}
	if !hasUpstream {
		argv = append(argv, "--set-upstream")
	}
	// The refspec is constructed here: no "+" prefix, no "--force", and no
	// way for the caller to name a different source or destination.
	// Combined with the ancestry check above, a non-fast-forward push
	// cannot happen.
	argv = append(argv, remote, "HEAD:refs/heads/"+branch)
	return argv, nil
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

// urlAllowed reports whether norm is in the allowlist, normalising each
// entry the same way so the two sides need not be spelled alike.
func urlAllowed(norm string, allowed []string) bool {
	for _, a := range allowed {
		if normalizeRemoteURL(a) == norm {
			return true
		}
	}
	return false
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

// runGitSafeCommit implements `git-safe commit`. It builds the argv via
// planCommit, which is pure and unit-tested, then refuses a detached
// HEAD for the same reason push does: the commit would land on no
// branch. Everything else is git's business.
func runGitSafeCommit(args []string) int {
	argv, err := planCommit(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 2
	}
	if _, err := gitOutput("symbolic-ref", "--quiet", "--short", "HEAD"); err != nil {
		fmt.Fprintln(os.Stderr, "git-safe: refusing to commit: HEAD is detached (check out a branch first)")
		return 1
	}
	return execGit(argv)
}

// planCommit is the entire policy for `git-safe commit`, kept pure so it
// can be tested without a repository. args is everything after the
// subcommand; the only accepted form is one or more `-m <message>`
// pairs, and repeated -m adds paragraphs exactly as it does for git.
//
// Signing is not the caller's choice: the wrapper always emits -S,
// because the host keyring the sandbox hides is the reason to route a
// commit through the broker, and because a fixed argv is what keeps the
// broker rule from having to describe the flags we do not want.
func planCommit(args []string) ([]string, error) {
	var msgs []string
	for i := 0; i < len(args); i++ {
		if args[i] != "-m" {
			return nil, fmt.Errorf("refusing to commit: %q is not allowed (only -m <message> is accepted)", args[i])
		}
		if i+1 == len(args) {
			return nil, fmt.Errorf("refusing to commit: -m needs a message")
		}
		i++
		// The value is taken verbatim, so a message that looks like a flag
		// ("--amend") stays a message — argv reaches git without a shell.
		msgs = append(msgs, args[i])
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("refusing to commit: a message is required (-m <message>)")
	}
	argv := []string{"git", "commit", "-S"}
	for _, m := range msgs {
		argv = append(argv, "-m", m)
	}
	return argv, nil
}

// gitOutput runs git and returns its combined output. Callers only care
// whether it succeeded.
func gitOutput(args ...string) (string, error) {
	out, err := exec.Command("git", args...).CombinedOutput()
	return string(out), err
}

// execGit runs the argv produced by planPush with the caller's stdio,
// streaming output as a normal shell would, and propagates the exit code.
func execGit(argv []string) int {
	cmd := exec.Command(argv[0], argv[1:]...)
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
