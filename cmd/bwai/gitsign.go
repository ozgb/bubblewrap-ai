package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Commit signing is split the same way git-safe is. Inside the sandbox,
// git's gpg.program and gpg.ssh.program point at the bwai-gpg persona,
// which forwards the buffer git wants signed to the broker as
// ["git-sign"]. On the host, `bwai git-sign` checks that the buffer is a
// commit and signs it with the host's own key.
//
// The host never runs git in the agent's repository to do this, which is
// the point: a repository the agent can write carries hooks and config
// (core.hooksPath, core.fsmonitor, gpg.program, filter drivers, …) that
// git executes, so any host-side git command in that tree runs the
// agent's code with the host's credentials.

// maxSignPayload bounds the buffer git-sign will sign. Commit headers are
// small; the message is the only part that can grow.
const maxSignPayload = 1 << 20

// runGpgShim is the sandbox-side bwai-gpg persona. git invokes it as its
// signing program, either gpg-style (`--status-fd=2 -bsau <key>`, buffer
// on stdin, armored signature on stdout) or ssh-keygen-style
// (`-Y sign -n git -f <key> <file>`, signature written to <file>.sig).
// Whichever key git names is ignored: the host picks its own.
func runGpgShim(args []string) int {
	if i := indexOf(args, "-Y"); i >= 0 {
		if i+1 < len(args) && args[i+1] == "sign" && len(args) > i+2 {
			return shimSSHSign(args[len(args)-1])
		}
		fmt.Fprintln(os.Stderr, "bwai-gpg: signature verification is not available inside the bwai sandbox")
		return 1
	}
	if indexOf(args, "--verify") >= 0 {
		fmt.Fprintln(os.Stderr, "bwai-gpg: signature verification is not available inside the bwai sandbox")
		return 1
	}
	if !gpgDetachSign(args) {
		fmt.Fprintf(os.Stderr, "bwai-gpg: unsupported invocation %q; only git's detached signing is forwarded\n", args)
		return 2
	}
	payload, err := io.ReadAll(io.LimitReader(os.Stdin, maxSignPayload+1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai-gpg: read payload: %v\n", err)
		return 1
	}
	return outsideExec([]string{"git-sign"}, payload, os.Stdout, os.Stderr)
}

// shimSSHSign forwards an ssh-keygen -Y sign request, writing the
// signature where ssh-keygen would have.
func shimSSHSign(file string) int {
	payload, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai-gpg: read payload: %v\n", err)
		return 1
	}
	var sig bytes.Buffer
	if code := outsideExec([]string{"git-sign"}, payload, &sig, os.Stderr); code != 0 {
		return code
	}
	if err := os.WriteFile(file+".sig", sig.Bytes(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "bwai-gpg: write signature: %v\n", err)
		return 1
	}
	return 0
}

// gpgDetachSign reports whether args ask gpg for a detached signature,
// the only operation git needs from gpg.program when signing.
func gpgDetachSign(args []string) bool {
	for _, a := range args {
		if a == "--detach-sign" {
			return true
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") &&
			strings.Contains(a, "b") && strings.Contains(a, "s") {
			return true
		}
	}
	return false
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// runGitSign is the host half: the `bwai git-sign` subcommand the broker
// runs for a ["git-sign"] request. It reads the buffer from stdin and
// writes the signature to stdout in the format git expects for the
// host's gpg.format.
func runGitSign(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "git-sign: takes no arguments; the buffer to sign is read from stdin")
		return 2
	}
	payload, err := io.ReadAll(io.LimitReader(os.Stdin, maxSignPayload+1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "git-sign: read payload: %v\n", err)
		return 1
	}
	if len(payload) > maxSignPayload {
		fmt.Fprintf(os.Stderr, "git-sign: refusing to sign: buffer exceeds %d bytes\n", maxSignPayload)
		return 1
	}
	if err := validateCommitPayload(payload); err != nil {
		fmt.Fprintf(os.Stderr, "git-sign: refusing to sign: %v\n", err)
		return 1
	}
	sc := loadSigningConfig(hostGitConfig)
	var argv []string
	var sigFile string
	switch sc.Format {
	case "", "openpgp":
		argv = []string{sc.Program, "--status-fd=2", "-bsa"}
		if sc.Key != "" {
			argv = append(argv, "-u", sc.Key)
		}
	case "ssh":
		if sc.Key == "" {
			fmt.Fprintln(os.Stderr, "git-sign: gpg.format is ssh but user.signingkey is not set on the host")
			return 1
		}
		dir, err := os.MkdirTemp("", "bwai-git-sign-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "git-sign: %v\n", err)
			return 1
		}
		defer os.RemoveAll(dir)
		buf := filepath.Join(dir, "buffer")
		if err := os.WriteFile(buf, payload, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "git-sign: %v\n", err)
			return 1
		}
		keyFile, literal := sshKeyArg(sc.Key)
		if literal {
			keyFile = filepath.Join(dir, "key.pub")
			if err := os.WriteFile(keyFile, []byte(strings.TrimPrefix(sc.Key, "key::")+"\n"), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "git-sign: %v\n", err)
				return 1
			}
		}
		argv = []string{sc.Program, "-Y", "sign", "-n", "git", "-f", keyFile}
		if literal {
			argv = append(argv, "-U")
		}
		argv = append(argv, buf)
		sigFile = buf + ".sig"
	default:
		fmt.Fprintf(os.Stderr, "git-sign: gpg.format %q is not supported\n", sc.Format)
		return 1
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = "/"
	cmd.Stderr = os.Stderr
	if sigFile == "" {
		cmd.Stdin = bytes.NewReader(payload)
		cmd.Stdout = os.Stdout
	}
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "git-sign: %s: %v\n", argv[0], err)
		return 1
	}
	if sigFile != "" {
		sig, err := os.ReadFile(sigFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "git-sign: read signature: %v\n", err)
			return 1
		}
		_, _ = os.Stdout.Write(sig)
	}
	return 0
}

// signingConfig is the host's choice of signing format, key and program.
type signingConfig struct {
	Format  string
	Key     string
	Program string
}

// loadSigningConfig resolves the signing setup the way git would, from
// host config only.
func loadSigningConfig(get func(key string) string) signingConfig {
	sc := signingConfig{Format: get("gpg.format"), Key: get("user.signingkey")}
	switch sc.Format {
	case "ssh":
		sc.Program = firstNonEmpty(get("gpg.ssh.program"), "ssh-keygen")
	default:
		sc.Program = firstNonEmpty(get("gpg.openpgp.program"), get("gpg.program"), "gpg")
	}
	return sc
}

// hostGitConfig reads one value from the host's system and global git
// config. It runs from / so no repository's config is consulted.
func hostGitConfig(key string) string {
	cmd := exec.Command("git", "config", "--get", key)
	cmd.Dir = "/"
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// sshKeyArg reports how git would pass user.signingkey to ssh-keygen: a
// literal public key (key:: prefix, or a bare "ssh-…" key) goes through a
// temp file with -U, anything else is a path.
func sshKeyArg(key string) (path string, literal bool) {
	if strings.HasPrefix(key, "key::") || strings.HasPrefix(key, "ssh-") {
		return "", true
	}
	if strings.HasPrefix(key, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, key[2:]), false
		}
	}
	return key, false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// commitHeaderRe matches one header line of a commit object as git hands
// it to the signer: a known name, a space, and a value.
var commitHeaderRe = regexp.MustCompile(`^(tree|parent|author|committer|encoding|mergetag) .+$`)

var objectIDRe = regexp.MustCompile(`^tree ([0-9a-f]{40}|[0-9a-f]{64})$`)

// validateCommitPayload accepts only a commit object: tree first, then
// the known headers, then a blank line before the message. Anything else
// — a tag, arbitrary data, a commit carrying a header git would not
// write — is refused, so the host key is not a general-purpose signing
// oracle for the sandbox.
func validateCommitPayload(p []byte) error {
	head, _, ok := strings.Cut(string(p), "\n\n")
	if !ok {
		return fmt.Errorf("not a commit object (no header/message separator)")
	}
	lines := strings.Split(head, "\n")
	if !objectIDRe.MatchString(lines[0]) {
		return fmt.Errorf("not a commit object (first line is not a tree header)")
	}
	var author, committer bool
	inMergetag := false
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, " ") {
			if !inMergetag {
				return fmt.Errorf("unexpected continuation line in commit header")
			}
			continue
		}
		if !commitHeaderRe.MatchString(l) {
			return fmt.Errorf("unexpected commit header %q", strings.SplitN(l, " ", 2)[0])
		}
		name := strings.SplitN(l, " ", 2)[0]
		switch name {
		case "tree":
			return fmt.Errorf("duplicate tree header")
		case "author":
			author = true
		case "committer":
			committer = true
		}
		inMergetag = name == "mergetag"
	}
	if !author || !committer {
		return fmt.Errorf("commit is missing an author or committer header")
	}
	return nil
}

// sandboxShimPath is where the bwai-gpg persona is bound in the sandbox.
const sandboxShimPath = "/run/bwai/bin/bwai-gpg"

// sandboxGitConfigEnv points the sandbox's git at the signing shim for
// both formats. Environment config outranks every config file, so a
// repository cannot redirect signing elsewhere.
func sandboxGitConfigEnv() []string {
	pairs := [][2]string{
		{"gpg.program", sandboxShimPath},
		{"gpg.ssh.program", sandboxShimPath},
	}
	args := []string{"--setenv", "GIT_CONFIG_COUNT", fmt.Sprint(len(pairs))}
	for i, p := range pairs {
		args = append(args,
			"--setenv", fmt.Sprintf("GIT_CONFIG_KEY_%d", i), p[0],
			"--setenv", fmt.Sprintf("GIT_CONFIG_VALUE_%d", i), p[1],
		)
	}
	return args
}
