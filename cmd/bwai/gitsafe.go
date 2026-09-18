package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// gitSafeUsage is printed by `git-safe --help` and on argv errors.
const gitSafeUsage = `git-safe — a deliberately narrow git wrapper for the bwai broker.

Usage:
  git-safe push                     Push the current branch to origin, fast-forward only.
  git-safe commit -m <message> ...  Commit staged changes, GPG-signed.

git-safe push accepts no flags, no refspecs, and no remote argument. It
refuses a detached HEAD and the protected branches (main, master, trunk,
develop), and it never performs a non-fast-forward update: origin's tip
must be an ancestor of HEAD. The remote branch is created if it does not
exist yet.

git-safe commit takes only -m <message>, repeated for extra paragraphs,
and always signs with -S — the host's keyring is the reason to route a
commit through the broker in the first place. Every other git commit
spelling is refused: --amend, -a/--all, --no-verify, --author, -F, and
bare pathspecs.

The policy lives in code rather than in an allowlist pattern. The broker's
matcher cannot express "any force flag, in any position", so commands that
need that judgement live behind a wrapper and the broker is left to match
a short argv: ["git-safe", "push"] or ["git-safe", "commit", "**"].`

// protectedBranches may never be pushed through git-safe.
var protectedBranches = []string{"main", "master", "trunk", "develop"}

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
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "git-safe push: takes no arguments")
		return 2
	}

	branch, err := gitOutput("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-safe: refusing to push: HEAD is detached (check out a branch first)")
		return 1
	}
	branch = strings.TrimSpace(branch)

	if _, err := gitOutput("remote", "get-url", "origin"); err != nil {
		fmt.Fprintln(os.Stderr, "git-safe: refusing to push: no git remote named \"origin\"")
		return 1
	}

	// Does the branch already exist on the remote? `ls-remote --exit-code`
	// exits non-zero when nothing matched, so a nil error means it does.
	remoteExists := false
	fastForward := false
	if _, err := gitOutput("ls-remote", "--exit-code", "origin", "refs/heads/"+branch); err == nil {
		remoteExists = true
		// Bring the remote tip local so ancestry is decidable, then
		// require it to be an ancestor of HEAD. This is what makes a
		// destructive update impossible rather than merely discouraged.
		if _, ferr := gitOutput("fetch", "--quiet", "origin", "refs/heads/"+branch); ferr != nil {
			fmt.Fprintf(os.Stderr, "git-safe: cannot fetch origin/%s: %v\n", branch, ferr)
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

	argv, err := planPush(branch, remoteExists, fastForward, hasUpstream)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 1
	}
	return execGit(argv)
}

// planPush is the entire policy, kept pure so it can be tested without a
// repository. It returns the exact argv to run, or an error explaining the
// refusal.
//
//	remoteExists — origin already has refs/heads/<branch>
//	fastForward  — origin's tip is an ancestor of HEAD (only consulted
//	               when remoteExists is true)
//	hasUpstream  — the local branch already tracks a remote branch
func planPush(branch string, remoteExists, fastForward, hasUpstream bool) ([]string, error) {
	if branch == "" {
		return nil, fmt.Errorf("refusing to push: no branch is checked out")
	}
	for _, p := range protectedBranches {
		if branch == p {
			return nil, fmt.Errorf("refusing to push protected branch %q", branch)
		}
	}
	if remoteExists && !fastForward {
		return nil, fmt.Errorf("refusing to push %q: origin/%s is not an ancestor of HEAD (fetch and merge or rebase first)", branch, branch)
	}

	argv := []string{"git", "push"}
	if !hasUpstream {
		argv = append(argv, "--set-upstream")
	}
	// The refspec is constructed here: no "+" prefix, no "--force", and no
	// way for the caller to name a different source or destination.
	// Combined with the ancestry check above, a non-fast-forward push
	// cannot happen.
	argv = append(argv, "origin", "HEAD:refs/heads/"+branch)
	return argv, nil
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
