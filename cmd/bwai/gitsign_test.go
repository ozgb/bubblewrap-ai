package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testTree = "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n"

func TestValidateCommitPayload(t *testing.T) {
	ident := "author A <a@example.com> 1700000000 +0000\ncommitter A <a@example.com> 1700000000 +0000\n"
	cases := []struct {
		name    string
		payload string
		ok      bool
	}{
		{"root commit", testTree + ident + "\nmsg\n", true},
		{"with parents", testTree + "parent 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nparent 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" + ident + "\nmsg\n", true},
		{"sha256 tree", "tree " + strings.Repeat("a", 64) + "\n" + ident + "\nmsg\n", true},
		{"encoding header", testTree + ident + "encoding ISO-8859-1\n\nmsg\n", true},
		{"mergetag continuation", testTree + ident + "mergetag object abc\n type commit\n\nmsg\n", true},
		{"tag object", "object 4b825dc642cb6eb9a060e54bf8d69288fbee4904\ntype commit\ntag v1\ntagger A <a@example.com> 1700000000 +0000\n\nmsg\n", false},
		{"arbitrary data", "hello world\n", false},
		{"no message separator", testTree + ident, false},
		{"missing committer", testTree + "author A <a@example.com> 1700000000 +0000\n\nmsg\n", false},
		{"unknown header", testTree + ident + "x-evil 1\n\nmsg\n", false},
		{"stray continuation", testTree + " continued\n" + ident + "\nmsg\n", false},
		{"duplicate tree", testTree + testTree + ident + "\nmsg\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCommitPayload([]byte(tc.payload))
			if (err == nil) != tc.ok {
				t.Fatalf("validateCommitPayload() err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestLoadSigningConfig(t *testing.T) {
	cfg := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		name string
		in   map[string]string
		want signingConfig
	}{
		{"defaults", nil, signingConfig{Program: "gpg"}},
		{"gpg.program", map[string]string{"gpg.program": "gpg2", "user.signingkey": "K"}, signingConfig{Key: "K", Program: "gpg2"}},
		{"openpgp.program wins", map[string]string{"gpg.program": "gpg2", "gpg.openpgp.program": "gpg3"}, signingConfig{Program: "gpg3"}},
		{"ssh", map[string]string{"gpg.format": "ssh", "user.signingkey": "~/.ssh/id.pub"}, signingConfig{Format: "ssh", Key: "~/.ssh/id.pub", Program: "ssh-keygen"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := loadSigningConfig(cfg(tc.in)); got != tc.want {
				t.Fatalf("loadSigningConfig() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestGpgDetachSign(t *testing.T) {
	cases := map[string]bool{
		"--status-fd=2 -bsau KEY": true,
		"--detach-sign":           true,
		"--status-fd=2 -bsa":      true,
		"--verify sig -":          false,
		"--list-keys":             false,
		"-e -r someone":           false,
	}
	for args, want := range cases {
		if got := gpgDetachSign(strings.Fields(args)); got != want {
			t.Errorf("gpgDetachSign(%q) = %v, want %v", args, got, want)
		}
	}
}

// TestRunGitSign drives the host half against stub signing programs set in
// a throwaway global config, checking that the payload reaches the signer,
// the signature comes back on stdout, and non-commits never reach it.
func TestRunGitSign(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "called")
	gpg := filepath.Join(dir, "fake-gpg")
	writeScript(t, gpg, "cat > "+marker+"\n"+
		"echo '-----BEGIN PGP SIGNATURE-----'\n"+
		"echo \"args:$*\"\n"+
		"echo '-----END PGP SIGNATURE-----'\n"+
		"echo '[GNUPG:] SIG_CREATED D 1 8 00 1700000000 ABC' >&2\n")
	sshKeygen := filepath.Join(dir, "fake-ssh-keygen")
	writeScript(t, sshKeygen, `for last; do :; done
cp "$last" `+marker+`
printf 'SSHSIG:%s\n' "$*" > "$last.sig"
`)
	global := filepath.Join(dir, "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	setGlobal := func(pairs ...string) {
		t.Helper()
		_ = os.Remove(global)
		for i := 0; i < len(pairs); i += 2 {
			if out, err := exec.Command("git", "config", "--file", global, pairs[i], pairs[i+1]).CombinedOutput(); err != nil {
				t.Fatalf("git config: %v\n%s", err, out)
			}
		}
	}
	commit := testTree + "author A <a@example.com> 1700000000 +0000\ncommitter A <a@example.com> 1700000000 +0000\n\nmsg\n"

	t.Run("openpgp signs with the host key", func(t *testing.T) {
		setGlobal("gpg.program", gpg, "user.signingkey", "HOSTKEY")
		_ = os.Remove(marker)
		code, out, errOut := runWithStdin(t, commit, func() int { return runGitSign(nil) })
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errOut)
		}
		if !strings.Contains(out, "BEGIN PGP SIGNATURE") || !strings.Contains(out, "-u HOSTKEY") {
			t.Fatalf("stdout = %q, want a signature made with -u HOSTKEY", out)
		}
		if !strings.Contains(errOut, "SIG_CREATED") {
			t.Fatalf("stderr = %q, want the gpg status line git looks for", errOut)
		}
		if got, _ := os.ReadFile(marker); string(got) != commit {
			t.Fatalf("signer saw %q, want the commit buffer", got)
		}
	})

	t.Run("ssh writes the signature to stdout", func(t *testing.T) {
		setGlobal("gpg.format", "ssh", "gpg.ssh.program", sshKeygen, "user.signingkey", "ssh-ed25519 AAAA test")
		_ = os.Remove(marker)
		code, out, errOut := runWithStdin(t, commit, func() int { return runGitSign(nil) })
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errOut)
		}
		if !strings.HasPrefix(out, "SSHSIG:-Y sign -n git -f ") || !strings.Contains(out, " -U ") {
			t.Fatalf("stdout = %q, want an ssh-keygen -Y sign with a literal key", out)
		}
		if got, _ := os.ReadFile(marker); string(got) != commit {
			t.Fatalf("signer saw %q, want the commit buffer", got)
		}
	})

	t.Run("refuses a non-commit", func(t *testing.T) {
		setGlobal("gpg.program", gpg)
		_ = os.Remove(marker)
		code, _, _ := runWithStdin(t, "not a commit\n", func() int { return runGitSign(nil) })
		if code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the signer ran for a refused payload")
		}
	})

	t.Run("refuses arguments", func(t *testing.T) {
		code, _, _ := runWithStdin(t, commit, func() int { return runGitSign([]string{"-u", "OTHER"}) })
		if code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
	})
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runWithStdin runs fn with os.Stdin fed from input and returns its exit
// code and what it wrote to stdout and stderr.
func runWithStdin(t *testing.T, input string, fn func() int) (int, string, string) {
	t.Helper()
	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	outF, _ := os.CreateTemp(t.TempDir(), "stdout")
	errF, _ := os.CreateTemp(t.TempDir(), "stderr")
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = in, outF, errF
	code := fn()
	os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
	out, _ := os.ReadFile(outF.Name())
	errOut, _ := os.ReadFile(errF.Name())
	return code, string(out), string(errOut)
}
