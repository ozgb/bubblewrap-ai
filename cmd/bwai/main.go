package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	// argv[0] dispatch: when bwai is invoked as `bwai-outside` from
	// inside the sandbox (via the bind-mounted helper), route to the
	// broker client instead of the sandbox flow.
	if filepath.Base(os.Args[0]) == "bwai-outside" {
		os.Exit(runOutsideClient(os.Args[1:]))
	}
	// Host-side subcommand dispatch. Only the leading positional —
	// flag args (`--command`, `-c`, `--version`, etc.) still belong to
	// the default sandbox flow.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "approve":
			os.Exit(runApproveCLI(os.Args[2:]))
		case "broker":
			os.Exit(runBrokerCLI(os.Args[2:]))
		}
	}
	os.Exit(runSandbox())
}

func runSandbox() int {
	versionFlag := flag.Bool("version", false, "Print version and exit")
	dumpConfig := flag.Bool("dump-config", false, "Print the default configuration JSON and exit")
	configFlag := flag.String("config", "", "Path to a config file (overrides ~/.bwai.json)")
	commandFlag := flag.String("command", "", "Command to run inside the sandbox (overrides config and default)")
	flag.StringVar(commandFlag, "c", "", "Shorthand for --command")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("%s\n", version)
		return 0
	}

	if *dumpConfig {
		cfg := defaultConfig()
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "bwai: failed to encode config: %v\n", err)
			return 1
		}
		return 0
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai: cannot determine home directory: %v\n", err)
		return 1
	}
	currentDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai: cannot determine current directory: %v\n", err)
		return 1
	}

	configPath := filepath.Join(home, ".bwai.json")
	if *configFlag != "" {
		configPath = *configFlag
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai: warning: could not load %s: %v\n", configPath, err)
	}
	homeAllow = cfg.HomeAllow
	homeBlock = cfg.HomeBlock

	command := cfg.Command
	if *commandFlag != "" {
		command = []string{*commandFlag}
	}
	// Append any trailing args after -- to the resolved command
	command = append(command, flag.Args()...)

	// Optionally start the host-execution broker. The broker exposes
	// two sockets in /tmp/bwai-$PID/; broker.sock gets bind-mounted
	// into the sandbox below.
	var broker *Broker
	if cfg.Broker.Enabled {
		broker, err = NewBroker(cfg.Broker, currentDir, defaultAuditPath(home))
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwai: broker init failed: %v\n", err)
			return 1
		}
		if err := installBwaiOutsideHelper(broker.TmpDir()); err != nil {
			fmt.Fprintf(os.Stderr, "bwai: install helper: %v\n", err)
			_ = broker.Close()
			return 1
		}
		if err := installAgentMemoryFile(broker.TmpDir()); err != nil {
			fmt.Fprintf(os.Stderr, "bwai: install CLAUDE.md: %v\n", err)
			_ = broker.Close()
			return 1
		}
		go broker.Serve()
		defer broker.Close()
	}

	fmt.Printf("bwai: sandboxed in %s\n", currentDir)
	if broker != nil {
		fmt.Println("bwai: broker enabled — sandbox can call `bwai-outside <cmd>`; `bwai-outside --help` lists rules.")
		if url := broker.WebURL(); url != "" {
			fmt.Printf("bwai: web approval enabled on %s — per-request links arrive via desktop notification.\n", url)
		}
	}
	args := []string{
		// Clear the inherited environment; only whitelisted vars are passed through below
		"--clearenv",
	}
	for _, key := range cfg.EnvAllow {
		if val, ok := os.LookupEnv(key); ok {
			args = append(args, "--setenv", key, val)
		}
	}
	args = append(args,
		// Read-only OS tree
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/etc", "/etc",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--ro-bind", "/opt", "/opt",
		"--ro-bind", "/sys", "/sys",
		// Device nodes
		"--dev", "/dev",
	)
	args = append(args, shmMount()...)
	args = append(args,
		// Virtual filesystems
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
	)
	args = append(args, dnsMounts()...)
	// Home directory
	args = append(args, tmpfs(home)...)
	args = append(args, homeMounts(home)...)
	args = append(args,
		// Current directory
		"--bind", currentDir, currentDir,
		"--chdir", currentDir,
		// Namespace isolation
		"--die-with-parent",
	)
	if broker != nil {
		// Bind broker.sock to /run/bwai/broker.sock and the helper
		// binary to /run/bwai/bin/bwai-outside. approve.sock is
		// *not* bind-mounted — it's host-only. CLAUDE.md is exposed
		// so an agent started with `--add-dir /run/bwai` (and
		// CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1) learns
		// about bwai-outside from its memory bootstrap.
		args = append(args,
			"--bind", broker.BrokerSocketPath(), "/run/bwai/broker.sock",
			"--ro-bind", filepath.Join(broker.TmpDir(), "bin", "bwai-outside"), "/run/bwai/bin/bwai-outside",
			"--ro-bind", filepath.Join(broker.TmpDir(), "CLAUDE.md"), "/run/bwai/CLAUDE.md",
			"--setenv", "BWAI_BROKER_SOCKET", "/run/bwai/broker.sock",
			"--setenv", "PATH", os.Getenv("PATH")+":/run/bwai/bin",
		)
	}
	args = append(args, cfg.BwrapExtraArgs...)

	// Inject a minimal rcfile so PS1 is set after /etc/bashrc runs, without
	// creating any file at ~/.bashrc (which is blocked). Write to /tmp/bwai.sh
	// (inside the --tmpfs /tmp) and point bash at it via --rcfile
	var extraFiles []*os.File
	if len(command) == 1 && command[0] == "bash" {
		bashrcR, bashrcW, pipeErr := os.Pipe()
		if pipeErr == nil {
			_, _ = fmt.Fprint(bashrcW, "PS1='[🫧] > '\n")
			_ = bashrcW.Close()
			// ExtraFiles[0] becomes fd 3 (after stdin/stdout/stderr)
			extraFiles = append(extraFiles, bashrcR)
			args = append(args, "--file", "3", "/tmp/bwai.sh")
			command = append([]string{command[0], "--rcfile", "/tmp/bwai.sh"}, command[1:]...)
		}
	} else {
		// Upon goose starts, the parent process spawn some child process and then
		// dies, which caused the startup of the tool to fail if the sandbox is
		// running with --unshare-pid (which it does by default).
		// By running the command via "bash -i -c goose" the iteractive shell
		// prevents it from exiting and goose starts normally.
		command = []string{"bash", "-i", "-c", strings.Join(command, " ")}
	}

	args = append(args, command...)

	// Execute the bubblewrap command
	cmd := exec.Command(cfg.BwrapPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = extraFiles

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "bwai: %v\n", err)
		return 1
	}
	return 0
}

// agentMemoryFileContent is the CLAUDE.md fragment that bwai writes
// into the broker tmpdir. When Claude Code is started inside the
// sandbox with `--add-dir /run/bwai` and
// CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1, this file is loaded
// as part of its memory bootstrap, teaching the agent about the
// bwai-outside tool. Other agents that respect AGENTS.md / similar
// conventions can be wired up the same way.
const agentMemoryFileContent = `# bwai broker

This shell runs inside a bwai sandbox. The project working tree is
read-write and the network is reachable, but host-only credentials —
` + "`~/.gnupg`" + `, ` + "`~/.ssh`" + `, ` + "`~/.aws`" + `, ` + "`gh`" + `'s auth, etc. — are deliberately
not mounted into the sandbox.

` + "`bwai-outside`" + ` is a narrow escape hatch for commands that need those
host credentials. **Default to running commands directly.** Only reach
for ` + "`bwai-outside`" + ` when a command would otherwise fail because the
sandbox hides a credential it needs. Ordinary work on local files —
including reading, editing, and committing — does *not* need it.

Use ` + "`bwai-outside`" + ` when the command requires host-only state:

` + "```sh" + `
bwai-outside git commit -S -m "fix bug"   # signed commit — needs ~/.gnupg
bwai-outside git push                     # ssh push — needs ~/.ssh
bwai-outside gh pr create                 # needs host gh auth
` + "```" + `

Run directly (do *not* prefix with ` + "`bwai-outside`" + `) for ordinary work —
these all succeed inside the sandbox:

` + "```sh" + `
git status
git add -A
git commit -m "fix bug"          # unsigned commit; no host creds needed
git diff
make test
npm install
` + "```" + `

Heuristic: if a command only touches the project tree or the network,
run it directly. ` + "`bwai-outside`" + ` is for the small set of operations
that need a credential the sandbox deliberately hides.

- ` + "`bwai-outside --help`" + ` (or no args) — prints this help and the
  current rule list, including which commands are auto-allowed and
  which require human confirmation.
- ` + "`bwai-outside --list-rules`" + ` — just the rules.

If a command is denied, it isn't on the allowlist. Check
` + "`bwai-outside --list-rules`" + ` first rather than retrying.

Commands flagged ` + "`CONFIRM`" + ` will pause until the human approves them
via ` + "`bwai approve`" + ` on the host. Output from approved commands streams
back as it would from a normal shell.
`

// installAgentMemoryFile writes the CLAUDE.md fragment into the
// broker tmpdir. It's bind-mounted into the sandbox at
// /run/bwai/CLAUDE.md.
func installAgentMemoryFile(tmpDir string) error {
	dstPath := filepath.Join(tmpDir, "CLAUDE.md")
	return os.WriteFile(dstPath, []byte(agentMemoryFileContent), 0o644)
}

// installBwaiOutsideHelper places a copy of the running bwai binary
// into the broker tmpdir under bin/bwai-outside. A symlink would be
// simpler but bwrap follows symlinks at bind-mount time on the host —
// the helper has to be a regular file inside the tmpdir so it
// resolves cleanly inside the sandbox.
func installBwaiOutsideHelper(tmpDir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	dstPath := filepath.Join(tmpDir, "bin", "bwai-outside")
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}
