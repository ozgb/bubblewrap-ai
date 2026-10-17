package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// tagNameRe is a conservative subset of git's ref-name rules; anchoring on an
// alphanumeric keeps a flag or a refspec out of the tag slot. The rest of
// git's rules are left to `git check-ref-format`.
var tagNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]*$`)

// planPushTagArgs is pure: it accepts `[<remote>] <tag>`, defaulting the
// remote to origin.
func planPushTagArgs(args []string) (remote, tag string, err error) {
	switch len(args) {
	case 1:
		remote, tag = "origin", args[0]
	case 2:
		remote, tag = args[0], args[1]
	default:
		return "", "", fmt.Errorf("refusing to push a tag: expected [<remote>] <tag>")
	}
	if !remoteNameRe.MatchString(remote) {
		return "", "", fmt.Errorf("refusing to push a tag: %q is not a valid remote name", remote)
	}
	if !tagNameRe.MatchString(tag) {
		return "", "", fmt.Errorf("refusing to push a tag: %q is not a valid tag name", tag)
	}
	return remote, tag, nil
}

// runGitSafePushTag implements `git-safe push-tag`. Like push it never runs
// git in the agent's repository: the tag is fetched into the host mirror
// through upload-pack and pushed from there. The policy is planPushTag.
func runGitSafePushTag(args []string) int {
	remote, tag, err := planPushTagArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 2
	}
	check := exec.Command("git", "check-ref-format", "refs/tags/"+tag)
	check.Dir = "/"
	if check.Run() != nil {
		fmt.Fprintf(os.Stderr, "git-safe: refusing to push a tag: %q is not a valid tag name\n", tag)
		return 2
	}
	repo, target, err := resolvePushTarget(remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 1
	}
	dest := target.URL
	protected := protectedBranchPatterns(target)
	mirror, err := ensureMirror(dest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: mirror: %v\n", err)
		return 1
	}
	pid := os.Getpid()
	incoming := fmt.Sprintf("refs/bwai/incoming/%d", pid)
	trackingPrefix := fmt.Sprintf("refs/bwai/remote/%d/", pid)
	defer cleanupMirrorRefs(mirror, incoming, trackingPrefix)

	if out, err := fetchFromRepo(mirror, repo, "refs/tags/"+tag, incoming); err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: cannot read tag %q from the repository: %v\n%s", tag, err, out)
		return 1
	}
	obj, err := mirrorGit(mirror, "rev-parse", "--verify", "--quiet", incoming)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: tag %q did not arrive in the host mirror\n", tag)
		return 1
	}
	obj = strings.TrimSpace(obj)
	commit, err := mirrorGit(mirror, "rev-parse", "--verify", "--quiet", incoming+"^{commit}")
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: refusing to push tag %q: it does not point to a commit\n", tag)
		return 1
	}
	commit = strings.TrimSpace(commit)
	if typ, _ := mirrorGit(mirror, "cat-file", "-t", obj); strings.TrimSpace(typ) == "tag" {
		body, err := mirrorGit(mirror, "cat-file", "tag", obj)
		if err != nil {
			fmt.Fprintf(os.Stderr, "git-safe: cannot read tag %q: %v\n", tag, err)
			return 1
		}
		if name := annotatedTagName(body); name != tag {
			fmt.Fprintf(os.Stderr, "git-safe: refusing to push tag %q: the tag object names itself %q\n", tag, name)
			return 1
		}
	}

	remoteObj := ""
	if out, err := mirrorGit(mirror, "ls-remote", "--exit-code", dest, "refs/tags/"+tag); err == nil {
		if f := strings.Fields(out); len(f) > 0 {
			remoteObj = f[0]
		}
	}

	onProtected, err := commitOnProtectedBranch(mirror, dest, commit, protected, trackingPrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 1
	}

	argv, err := planPushTag(tag, remote, dest, obj, remoteObj, onProtected, target.TagPatterns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-safe: %v\n", err)
		return 1
	}
	if argv == nil {
		fmt.Printf("git-safe: tag %s is already on %s\n", tag, remote)
		return 0
	}
	if code := execGit(mirror, argv); code != 0 {
		return code
	}
	fmt.Printf("git-safe: pushed tag %s (%s) to %s\n", tag, commit[:12], remote)
	return 0
}

// planPushTag is the policy, kept pure like planPush. It returns the argv to
// run in the mirror, or nil when the remote already holds this exact tag.
//
//	obj         — the tag ref's target (tag object or commit), in the mirror
//	remoteObj   — what dest's refs/tags/<tag> points at, "" if absent
//	onProtected — the tagged commit is reachable from a protected branch on
//	              dest
//
// A tag is immutable once published, and may only name a commit that already
// reached a protected branch — which the agent cannot push to — so the
// agent can mark a release but not decide what goes into one.
func planPushTag(tag, remote, dest, obj, remoteObj string, onProtected bool, patterns []string) ([]string, error) {
	if !matchesAny(tag, patterns) {
		return nil, fmt.Errorf("refusing to push tag %q: it matches no tag_patterns of its allowed_remotes entry", tag)
	}
	if remoteObj == obj {
		return nil, nil
	}
	if remoteObj != "" {
		return nil, fmt.Errorf("refusing to push tag %q: %s already has it pointing elsewhere, and tags are never moved", tag, remote)
	}
	if !onProtected {
		return nil, fmt.Errorf("refusing to push tag %q: its commit is not on a protected branch of %s (merge it first)", tag, remote)
	}
	return []string{"git", "push", dest, obj + ":refs/tags/" + tag}, nil
}

// annotatedTagName returns the `tag` header of a tag object.
func annotatedTagName(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			break
		}
		if name, ok := strings.CutPrefix(line, "tag "); ok {
			return name
		}
	}
	return ""
}

// commitOnProtectedBranch fetches dest's protected branches into the mirror
// under prefix and reports whether any of them contains commit.
func commitOnProtectedBranch(mirror, dest, commit string, protected []string, prefix string) (bool, error) {
	out, err := mirrorGit(mirror, "ls-remote", "--heads", dest)
	if err != nil {
		return false, fmt.Errorf("cannot list branches of %s: %v\n%s", dest, err, out)
	}
	var refspecs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		branch, ok := strings.CutPrefix(f[1], "refs/heads/")
		if ok && matchesAny(branch, protected) {
			refspecs = append(refspecs, "+"+f[1]+":"+prefix+branch)
		}
	}
	if len(refspecs) == 0 {
		return false, nil
	}
	fetch := append([]string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", dest}, refspecs...)
	if out, err := mirrorGit(mirror, fetch...); err != nil {
		return false, fmt.Errorf("cannot fetch protected branches of %s: %v\n%s", dest, err, out)
	}
	refs, err := mirrorGit(mirror, "for-each-ref", "--contains", commit, "--format=%(refname)", prefix)
	if err != nil {
		return false, fmt.Errorf("cannot check where %s is: %v\n%s", commit, err, refs)
	}
	return strings.TrimSpace(refs) != "", nil
}

// cleanupMirrorRefs drops this run's scratch refs from the mirror.
func cleanupMirrorRefs(mirror, incoming, prefix string) {
	_, _ = mirrorGit(mirror, "update-ref", "-d", incoming)
	refs, _ := mirrorGit(mirror, "for-each-ref", "--format=%(refname)", prefix)
	for _, ref := range strings.Fields(refs) {
		_, _ = mirrorGit(mirror, "update-ref", "-d", ref)
	}
}
