package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// ghSafeUsage is printed by `gh-safe --help` and on argv errors.
const ghSafeUsage = `gh-safe — a deliberately narrow gh wrapper for the bwai broker.

Usage:
  gh-safe -R <owner/repo> blocked-by <issue>           Issues blocking <issue>
  gh-safe -R <owner/repo> block <issue> <blocker>      Mark <issue> blocked by <blocker>
  gh-safe -R <owner/repo> unblock <issue> <blocker>    Remove that dependency
  gh-safe -R <owner/repo> sub-issues <issue>           Sub-issues of <issue>
  gh-safe -R <owner/repo> add-sub <parent> <child>     Make <child> a sub-issue of <parent>
  gh-safe -R <owner/repo> remove-sub <parent> <child>  Detach <child> from <parent>

-R comes first and is required. Issues are plain numbers. Nothing else is
accepted, and nothing is passed through to gh.

These are calls gh only offers through ` + "`gh api`" + `, whose endpoint is a
single argument with the issue number inside it, so a broker rule over raw
gh api is either one rule per issue or open to every endpoint. gh-safe
builds the endpoint itself, which lets the rule pin the repository instead:
["gh-safe", "-R", "owner/repo", "**"].`

// ghRepoPattern is GitHub's owner/name shape. It also keeps the repository
// from smuggling extra path segments into the endpoint gh-safe builds.
var ghRepoPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+$`)

var ghIssuePattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

var ghIDPattern = regexp.MustCompile(`^[0-9]+$`)

// ghSafeOp is one gh-safe subcommand: its argument count and how it runs.
type ghSafeOp struct {
	nargs int
	run   func(g ghAPI, repo string, nums []string) error
}

var ghSafeOps = map[string]ghSafeOp{
	"blocked-by": {1, func(g ghAPI, repo string, n []string) error {
		return g.list("repos/" + repo + "/issues/" + n[0] + "/dependencies/blocked_by")
	}},
	"block": {2, func(g ghAPI, repo string, n []string) error {
		id, err := g.issueID(repo, n[1])
		if err != nil {
			return err
		}
		if err := g.write("POST", "repos/"+repo+"/issues/"+n[0]+"/dependencies/blocked_by", "issue_id="+id); err != nil {
			return err
		}
		fmt.Fprintf(g.out, "#%s is now blocked by #%s\n", n[0], n[1])
		return nil
	}},
	"unblock": {2, func(g ghAPI, repo string, n []string) error {
		id, err := g.issueID(repo, n[1])
		if err != nil {
			return err
		}
		if err := g.write("DELETE", "repos/"+repo+"/issues/"+n[0]+"/dependencies/blocked_by/"+id, ""); err != nil {
			return err
		}
		fmt.Fprintf(g.out, "#%s is no longer blocked by #%s\n", n[0], n[1])
		return nil
	}},
	"sub-issues": {1, func(g ghAPI, repo string, n []string) error {
		return g.list("repos/" + repo + "/issues/" + n[0] + "/sub_issues")
	}},
	"add-sub": {2, func(g ghAPI, repo string, n []string) error {
		id, err := g.issueID(repo, n[1])
		if err != nil {
			return err
		}
		if err := g.write("POST", "repos/"+repo+"/issues/"+n[0]+"/sub_issues", "sub_issue_id="+id); err != nil {
			return err
		}
		fmt.Fprintf(g.out, "#%s is now a sub-issue of #%s\n", n[1], n[0])
		return nil
	}},
	"remove-sub": {2, func(g ghAPI, repo string, n []string) error {
		id, err := g.issueID(repo, n[1])
		if err != nil {
			return err
		}
		// Singular sub_issue: GitHub's removal endpoint differs from the add/list one.
		if err := g.write("DELETE", "repos/"+repo+"/issues/"+n[0]+"/sub_issue", "sub_issue_id="+id); err != nil {
			return err
		}
		fmt.Fprintf(g.out, "#%s is no longer a sub-issue of #%s\n", n[1], n[0])
		return nil
	}},
}

// parseGhSafe validates argv and returns the repository, the operation and
// its issue numbers.
func parseGhSafe(args []string) (repo string, op ghSafeOp, nums []string, err error) {
	if len(args) < 3 || args[0] != "-R" {
		return "", op, nil, errors.New("expected -R <owner/repo> <subcommand> …")
	}
	repo = args[1]
	if !ghRepoPattern.MatchString(repo) {
		return "", op, nil, fmt.Errorf("not an owner/repo: %q", repo)
	}
	op, ok := ghSafeOps[args[2]]
	if !ok {
		return "", op, nil, fmt.Errorf("unknown subcommand %q", args[2])
	}
	nums = args[3:]
	if len(nums) != op.nargs {
		return "", op, nil, fmt.Errorf("%s takes %d issue number(s), got %d", args[2], op.nargs, len(nums))
	}
	for _, n := range nums {
		if !ghIssuePattern.MatchString(n) {
			return "", op, nil, fmt.Errorf("not an issue number: %q", n)
		}
	}
	return repo, op, nums, nil
}

// ghAPI runs `gh api`. exec is swapped out in tests.
type ghAPI struct {
	out  io.Writer
	exec func(stdout io.Writer, args ...string) error
}

func (g ghAPI) list(endpoint string) error {
	return g.exec(g.out, "api", "--paginate", endpoint, "--jq", `.[] | "#\(.number) \(.state) \(.title)"`)
}

func (g ghAPI) write(method, endpoint, field string) error {
	args := []string{"api", "--silent", "-X", method, endpoint}
	if field != "" {
		args = append(args, "-F", field)
	}
	return g.exec(g.out, args...)
}

// issueID resolves an issue number to the database id the dependency and
// sub-issue endpoints take in their bodies and paths.
func (g ghAPI) issueID(repo, num string) (string, error) {
	var buf bytes.Buffer
	if err := g.exec(&buf, "api", "repos/"+repo+"/issues/"+num, "--jq", ".id"); err != nil {
		return "", err
	}
	id := strings.TrimSpace(buf.String())
	if !ghIDPattern.MatchString(id) {
		return "", fmt.Errorf("unexpected id for #%s: %q", num, id)
	}
	return id, nil
}

func execGh(stdout io.Writer, args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runGhSafeClient is the sandbox-side half, reached as the `gh-safe`
// persona. Like git-safe's client it decides nothing: it forwards
// ["gh-safe", …] to the broker, which runs the host half.
func runGhSafeClient(args []string) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Println(ghSafeUsage)
		return 0
	}
	return runOutsideExec(append([]string{"gh-safe"}, args...))
}

// runGhSafe is the host-side half: the `bwai gh-safe …` subcommand the
// broker runs once a ["gh-safe", …] rule matches.
func runGhSafe(args []string) int {
	return ghSafe(args, ghAPI{out: os.Stdout, exec: execGh})
}

func ghSafe(args []string, g ghAPI) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Fprintln(g.out, ghSafeUsage)
		return 0
	}
	repo, op, nums, err := parseGhSafe(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh-safe: %v\n", err)
		fmt.Fprintln(os.Stderr, ghSafeUsage)
		return 2
	}
	if err := op.run(g, repo, nums); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "gh-safe: %v\n", err)
		return 1
	}
	return 0
}
