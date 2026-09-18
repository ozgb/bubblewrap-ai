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
  git-safe push     Push the current branch to origin, fast-forward only.

git-safe accepts no flags, no refspecs, and no remote argument. It refuses
a detached HEAD and the protected branches (main, master, trunk, develop),
and it never performs a non-fast-forward update: origin's tip must be an
ancestor of HEAD. The remote branch is created if it does not exist yet.

The policy lives in code rather than in an allowlist pattern. The broker's
matcher cannot express "any force flag, in any position", so commands that
need that judgement live behind a wrapper and the broker is left to match a
two-token argv: ["git-safe", "push"].`

// protectedBranches may never be pushed through git-safe.
var protectedBranches = []string{"main", "master", "trunk", "develop"}

// runGitSafe is the entry point when the binary is invoked as `git-safe`.
func runGitSafe(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "git-safe: missing subcommand")
		fmt.Fprintln(os.Stderr, gitSafeUsage)
		return 2
	}
	switch args[0] {
	case "push":
		return runGitSafePush(args[1:])
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
