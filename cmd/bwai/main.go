package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	// Both bind-mounted helpers are the same binary under a different
	// argv[0]; name the client after whichever one was invoked so its
	// errors are not reported under the other's name.
	prog := filepath.Base(os.Args[0])
	if prog == "bwai-outside" || prog == "git-safe" {
		outsideProg = prog
	}
	// argv[0] dispatch: when bwai is invoked as `bwai-outside` from
	// inside the sandbox (via the bind-mounted helper), route to the
	// broker client instead of the sandbox flow.
	if prog == "bwai-outside" {
		os.Exit(runOutsideClient(os.Args[1:]))
	}
	// `git-safe` is a second argv[0] persona, installed next to
	// bwai-outside under /run/bwai/bin (see the bind mounts below). It is
	// only the client half: the policy runs on the host via the
	// `bwai git-safe` subcommand, which is the only thing the broker
	// resolves it to.
	if prog == "git-safe" {
		os.Exit(runGitSafeClient(os.Args[1:]))
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
		case "git-safe":
			// The host half of the git-safe wrapper. The broker runs
			// this itself (see hostArgv); it is also reachable by hand,
			// which is what replaced the old ~/.local/bin/git-safe
			// symlink.
			os.Exit(runGitSafe(os.Args[2:]))
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
	// A .bwai.json in the sandbox root layers on top of the global config:
	// its list fields are appended, everything else overrides. Skip it if
	// it is the base itself (e.g. running from $HOME).
	localPath := filepath.Join(currentDir, ".bwai.json")
	if localPath == configPath {
		localPath = ""
	}
	cfg, err := loadLayeredConfig(configPath, localPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai: warning: could not load config: %v\n", err)
	}
	homeAllow = cfg.HomeAllow
	homeBlock = cfg.HomeBlock

	// Persistent state root: created on the host and bound read-write below
	// so tool caches and installed binaries survive the session without
	// exposing the host's own caches.
	stateRoot := expandHome(cfg.StateRoot, home)
	if stateRoot != "" {
		if err := os.MkdirAll(filepath.Join(stateRoot, "bin"), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "bwai: warning: state_root %s: %v\n", stateRoot, err)
			stateRoot = ""
		}
	}

	command := cfg.Command
	if *commandFlag != "" {
		command = []string{*commandFlag}
	}
	// Append any trailing args after -- to the resolved command
	command = append(command, flag.Args()...)

	// Worktrees created with `wt` default to a sibling of the repo, which
	// would land on the sandbox's tmpfs home and vanish with the session.
	// Bind a dedicated host root and point worktrunk at it below. Computed
	// before the broker so the agent memory below can name the path.
	// Starting in a linked worktree resolves the same root via the main
	// tree, so all sessions of a repo share one root.
	var startedInWorktree bool
	var worktreeMainTree string
	exposeMain := cfg.Worktrees == nil || cfg.Worktrees.ExposeMain
	worktreeMounts, worktreeRoot, err := worktreeRootMounts(currentDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai: warning: could not prepare worktree root: %v\n", err)
		worktreeMounts, worktreeRoot = nil, ""
	}
	if worktreeRoot == "" {
		var mainTree string
		siblingMounts, siblingRoot, mainTree, serr := worktreeSiblingMounts(currentDir, exposeMain)
		if serr != nil {
			fmt.Fprintf(os.Stderr, "bwai: warning: could not prepare worktree root: %v\n", serr)
			siblingMounts, siblingRoot = nil, ""
		}
		if siblingRoot != "" {
			startedInWorktree = true
			worktreeMainTree = mainTree
		}
		worktreeMounts, worktreeRoot = siblingMounts, siblingRoot
	}

	// Optionally start the host-execution broker. The broker exposes
	// two sockets in /tmp/bwai-$PID/; broker.sock gets bind-mounted
	// into the sandbox below.
	var broker *Broker
	if cfg.Broker.Enabled {
		// Every root the sandbox got rw is accepted as request cwd.
		// currentDir is always revealed; expose_main adds the main tree.
		roots := []string{worktreeRoot}
		if startedInWorktree && exposeMain && worktreeMainTree != "" {
			roots = append(roots, worktreeMainTree)
		}
		broker, err = NewBroker(cfg.Broker, currentDir, defaultAuditPath(home), roots...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwai: broker init failed: %v\n", err)
			return 1
		}
		if err := installBwaiOutsideHelper(broker.TmpDir()); err != nil {
			fmt.Fprintf(os.Stderr, "bwai: install helper: %v\n", err)
			_ = broker.Close()
			return 1
		}
		if err := installAgentMemoryFile(broker.TmpDir(), cfg.Broker.Rules, worktreeRoot, worktreeMainTree, exposeMain); err != nil {
			fmt.Fprintf(os.Stderr, "bwai: install agent memory: %v\n", err)
			_ = broker.Close()
			return 1
		}
		if err := installBwaiMod(broker.TmpDir()); err != nil {
			fmt.Fprintf(os.Stderr, "bwai: install command-code mod: %v\n", err)
			_ = broker.Close()
			return 1
		}
		if err := installOpencodeConfig(broker.TmpDir()); err != nil {
			fmt.Fprintf(os.Stderr, "bwai: install opencode config: %v\n", err)
			_ = broker.Close()
			return 1
		}
		go broker.Serve()
		defer broker.Close()
	}

	// Linked git worktrees keep their real git dir inside the main repo,
	// which the home --tmpfs hides. Detect that and bind it back in.
	gitMounts := gitWorktreeMounts(currentDir)

	fmt.Printf("bwai: sandboxed in %s\n", currentDir)
	if len(gitMounts) > 0 {
		fmt.Println("bwai: detected a git worktree — exposing its git dir so history and commits work.")
	}
	if worktreeRoot != "" {
		fmt.Printf("bwai: git worktrees persist on the host at %s\n", worktreeRoot)
		if startedInWorktree {
			if exposeMain {
				fmt.Println("bwai: main checkout is exposed read-write.")
			} else {
				fmt.Println("bwai: main checkout is hidden (worktrees.expose_main=false).")
			}
		}
	}
	if broker != nil {
		fmt.Println("bwai: broker enabled — sandbox can call `bwai-outside <cmd>`; `bwai-outside --help` lists rules.")
		if url := broker.WebURL(); url != "" {
			fmt.Printf("bwai: web approval enabled on %s — per-request links arrive via desktop notification.\n", url)
		}
		fmt.Println("bwai: command-code can load the bwai context with `cmd --mod /run/bwai/bwai.ts`.")
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
	// The state-root bundle, then literal env_set. bwrap applies --setenv in
	// order, so env_set overrides the bundle and bwrap_extra_args overrides
	// both.
	if stateRoot != "" {
		args = append(args, stateRootEnvArgs(stateRoot)...)
	}
	for _, key := range sortedKeys(cfg.EnvSet) {
		args = append(args, "--setenv", key, expandHome(cfg.EnvSet[key], home))
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
	args = append(args, gpuMounts()...)
	args = append(args, shmMount()...)
	args = append(args,
		// Virtual filesystems
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
	)
	args = append(args, dnsMounts()...)
	args = append(args, etcResolvMount()...)
	// Home directory
	args = append(args, tmpfs(home)...)
	args = append(args, homeMounts(home)...)
	// Bind the state root after homeMounts, which would otherwise ro-bind a
	// dotdir under $HOME before this re-binds it read-write.
	if stateRoot != "" {
		args = append(args, rwBind(stateRoot)...)
	}
	args = append(args,
		// Current directory
		"--bind", currentDir, currentDir,
		"--chdir", currentDir,
		// Namespace isolation
		"--die-with-parent",
	)
	// Expose the shared git dir for linked worktrees. Must come after the
	// home --tmpfs so bwrap creates the mountpoint inside the tmpfs, the
	// same ordering homeMounts relies on for dotfiles.
	args = append(args, gitMounts...)
	args = append(args, worktreeMounts...)
	if worktreeRoot != "" {
		// worktrunk reads the full worktree path from this template and
		// overrides ~/.config/worktrunk/config.toml, which is read-only here.
		args = append(args, "--setenv", "WORKTRUNK_WORKTREE_PATH",
			filepath.Join(worktreeRoot, "{{ branch | sanitize }}"))
	}
	if broker != nil {
		// Bind broker.sock to /run/bwai/broker.sock and the helper
		// binary under /run/bwai/bin. approve.sock is *not*
		// bind-mounted — it's host-only. The context fragment and mod
		// are exposed read-only so an agent can opt into them without
		// bwai writing to the agent's own config.
		//
		// One copy of this binary, two names: the sandbox picks its
		// persona from argv[0], so binding the same regular file at both
		// destinations gives the agent `bwai-outside` and `git-safe`
		// without either being installed on the host. (A symlink would be
		// cheaper but bwrap resolves symlinks at bind time on the host,
		// so the source has to be a real file.)
		helper := filepath.Join(broker.TmpDir(), "bin", "bwai-outside")
		args = append(args,
			"--bind", broker.BrokerSocketPath(), "/run/bwai/broker.sock",
			"--ro-bind", helper, "/run/bwai/bin/bwai-outside",
			"--ro-bind", helper, "/run/bwai/bin/git-safe",
			"--ro-bind", filepath.Join(broker.TmpDir(), "CLAUDE.md"), "/run/bwai/CLAUDE.md",
			"--ro-bind", filepath.Join(broker.TmpDir(), "bwai.ts"), "/run/bwai/bwai.ts",
			"--ro-bind", filepath.Join(broker.TmpDir(), "opencode.json"), "/run/bwai/opencode.json",
			"--setenv", "OPENCODE_CONFIG", "/run/bwai/opencode.json",
			"--setenv", "BWAI_BROKER_SOCKET", "/run/bwai/broker.sock",
		)
	}
	// PATH: user prepends, then the state root's bin, then the broker's
	// helper dir — which must beat a stale host git-safe, so it precedes the
	// host PATH. One --setenv keeps the parts from clobbering each other.
	var prepend []string
	for _, p := range cfg.PathPrepend {
		if p = expandHome(p, home); p != "" {
			prepend = append(prepend, p)
		}
	}
	if stateRoot != "" {
		prepend = append(prepend, filepath.Join(stateRoot, "bin"))
	}
	if broker != nil {
		prepend = append(prepend, "/run/bwai/bin")
	}
	if len(prepend) > 0 {
		args = append(args, "--setenv", "PATH", strings.Join(prepend, ":")+":"+os.Getenv("PATH"))
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

	// A home_block entry naming a file (e.g. .cargo/credentials.toml) can't
	// be hidden with --tmpfs, so replace it with an empty --file. Each needs
	// its own fd: bwrap consumes the one it is given and rejects reuse.
	for _, p := range blockedSubPathFiles(home, homeBlock, homeAllow) {
		maskR, maskW, pipeErr := os.Pipe()
		if pipeErr != nil {
			continue
		}
		_ = maskW.Close()
		extraFiles = append(extraFiles, maskR)
		args = append(args, "--file", fmt.Sprintf("%d", 2+len(extraFiles)), p)
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

// stateRootEnv points each tool's cache or config home at a subdirectory of
// the configured state_root. "." means the root itself: cargo installs its
// binaries into <root>/bin via CARGO_INSTALL_ROOT, matching the UV_*_BIN_DIR
// entries.
var stateRootEnv = map[string]string{
	"XDG_CACHE_HOME":           "cache",
	"npm_config_cache":         "cache/npm",
	"YARN_CACHE_FOLDER":        "cache/yarn",
	"PIP_CACHE_DIR":            "cache/pip",
	"UV_CACHE_DIR":             "cache/uv",
	"UV_PYTHON_INSTALL_DIR":    "uv/python",
	"UV_PYTHON_BIN_DIR":        "bin",
	"UV_TOOL_DIR":              "uv/tools",
	"UV_TOOL_BIN_DIR":          "bin",
	"CARGO_HOME":               "cargo",
	"CARGO_INSTALL_ROOT":       ".",
	"GOMODCACHE":               "go/pkg/mod",
	"GOCACHE":                  "go/build",
	"GOBIN":                    "bin",
	"BUN_INSTALL_CACHE_DIR":    "cache/bun",
	"PLAYWRIGHT_BROWSERS_PATH": "cache/ms-playwright",
	"HF_HOME":                  "cache/huggingface",
}

// stateRootEnvArgs renders the bundle as bwrap --setenv args in a stable
// order, since map iteration is not deterministic.
func stateRootEnvArgs(root string) []string {
	var args []string
	for _, key := range sortedKeys(stateRootEnv) {
		args = append(args, "--setenv", key, filepath.Join(root, stateRootEnv[key]))
	}
	return args
}

// expandHome expands a leading ~ (or a bare ~) against the sandbox home.
// bwrap expands neither ~ nor $VAR in --setenv or bind paths, so bwai does
// the ~ expansion itself.
func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// sortedKeys returns a map's keys sorted, so generated argv is stable.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
bwai-outside gh pr create   # needs host gh auth
` + "```" + `

The two git operations that need host credentials have their own command,
` + "`git-safe`" + `. It is already on your ` + "`PATH`" + ` — call it directly, with
no ` + "`bwai-outside`" + ` prefix:

` + "```sh" + `
git-safe commit -m "fix bug"  # commits what is staged, GPG-signed
git-safe push                 # publishes the current branch (fast-forward only)
` + "```" + `

` + "`bwai-outside git push`" + ` is deliberately not allowed. ` + "`git-safe push`" + `
pushes the current branch to ` + "`origin`" + ` and refuses force, the protected
branches (main/master/trunk/develop), and any non-fast-forward update.

` + "`git-safe commit`" + ` is the signing path: it takes only ` + "`-m <message>`" + `
(repeat it for extra paragraphs), always signs, and refuses every other
spelling — no ` + "`--amend`" + `, no ` + "`-a`" + `, no ` + "`--no-verify`" + `, no pathspecs. Use it
instead of ` + "`bwai-outside git commit -S`" + `, which exposes the whole flag
surface to the broker.

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
- ` + "`bwai-outside --check <cmd> [args...]`" + ` — dry-run: prints which
  rule would match the argv and what the broker would do, runs nothing.

If a command is denied, the message names the rule that fired, e.g.
"denied (rule) — rules[12] AUTO_DENY gh secret **" — read it before
retrying a variant, since the fix is often a small change in argv, like
moving a flag. Check ` + "`bwai-outside --list-rules`" + ` first rather than
retrying blind; when unsure which rule a spelling would hit,
` + "`bwai-outside --check`" + ` answers without running anything.

Commands flagged ` + "`AUTO_ALLOW`" + ` need no approval — run them directly,
there is nothing to wait for. ` + "`CONFIRM`" + ` commands pause until the human
approves them via ` + "`bwai approve`" + ` on the host. Output from approved
commands streams back as it would from a normal shell.

**Hunt for an ` + "`AUTO_ALLOW`" + ` rule; never settle for ` + "`CONFIRM`" + `
while one exists.** A confirm stalls the whole session on a human who may
be asleep, so treat every approval prompt as a failure to find the rule
that already covers the job. Scan the rule list for the wrapper or the
narrower argv that is already auto-allowed — ` + "`git-safe push`" + ` over a
confirmed raw ` + "`git push`" + `, ` + "`git-safe commit`" + ` over
` + "`bwai-outside git commit -S`" + `. Taking the first rule that happens to
match, when an auto-allowed one sits a line below it, is laziness that
spends a human's attention on nothing.

The same convention governs writing rules: a command whose safety is
guaranteed in code by a closed wrapper gets ` + "`auto_allow`" + `, never
` + "`confirm`" + `. The wrapper has already made the judgement a human would
have made, so the prompt adds nothing — and an approval that is always
granted is worse than none, because it trains the habit of approving
without reading. ` + "`confirm`" + ` is the last resort, reserved for a check
no wrapper can express; before you write one, ask what wrapper would make
it unnecessary.
`

// worktreeSectionTemplate is the ## Git worktrees section, rendered only
// when bwai bound a persistent worktree root. {{worktree_root}} is
// substituted with that host path. {{writable_line}} states which paths
// are writable, and differs when the session started in a linked
// worktree (where the main checkout may or may not also be exposed).
const worktreeSectionTemplate = `
## Git worktrees

{{writable_line}} Bind-mounted from the host,
anything created there survives the sandbox; everything else outside the
project tree lives on tmpfs and is gone when the session ends.

Create worktrees there — with ` + "`wt`" + ` (which bwai already points at this
directory through ` + "`WORKTRUNK_WORKTREE_PATH`" + `), or with plain git:

` + "```sh" + `
git worktree add {{worktree_root}}/mytask -b mytask
wt switch --create mytask        # lands in the same directory
` + "```" + `

A bare ` + "`git worktree add ../foo`" + ` does *not* follow that setting: ` + "`../foo`" + `
is a sibling of the project on tmpfs, so the worktree vanishes with the
sandbox and the registration left in ` + "`.git/worktrees`" + ` goes stale. Give
the destination under ` + "`{{worktree_root}}`" + ` explicitly.
`

// renderWorktreeSection fills the worktree section with the bound root.
// mainTree, when non-empty, is the main checkout this worktree belongs
// to; exposeMain says whether it is mounted read-write this session.
func renderWorktreeSection(root, mainTree string, exposeMain bool) string {
	var line string
	switch {
	case mainTree == "":
		line = "The only directory outside the project tree you can write to is `" + root + "`."
	case exposeMain:
		line = "The only directories outside the project tree you can write to are `" + root + "` and the main checkout at `" + mainTree + "`."
	default:
		line = "The only directory outside the project tree you can write to is `" + root + "`; the main checkout is not exposed (worktrees.expose_main=false)."
	}
	s := strings.ReplaceAll(worktreeSectionTemplate, "{{worktree_root}}", root)
	return strings.ReplaceAll(s, "{{writable_line}}", line)
}

// installAgentMemoryFile writes the CLAUDE.md fragment into the broker
// tmpdir. It's bind-mounted into the sandbox at /run/bwai/CLAUDE.md,
// where Claude Code picks it up via `--add-dir /run/bwai`, and where the
// command-code mod below reads it.
//
// The live rule set is rendered into the fragment so every agent that
// loads it knows what the broker will and won't run before its first
// turn — rather than depending on the model choosing to run
// `bwai-outside -h` (which the prose below already suggests, and which
// models routinely skip). printRules is shared with `--list-rules`, so
// the injected view and the on-demand view cannot drift apart.
//
// worktreeRoot, when non-empty, names the bind-mounted host directory
// where a worktree persists; it is rendered into the fragment so agents
// don't create one on the ephemeral overlay and lose it. mainTree is
// set only for sessions that started in a linked worktree ("" for main
// checkouts) and selects the writable-paths wording via exposeMain.
func installAgentMemoryFile(tmpDir string, rules []Rule, worktreeRoot, mainTree string, exposeMain bool) error {
	var b strings.Builder
	b.WriteString(agentMemoryFileContent)
	if worktreeRoot != "" {
		b.WriteString(renderWorktreeSection(worktreeRoot, mainTree, exposeMain))
	}
	b.WriteString("\n## Broker rules for this sandbox\n\n")
	b.WriteString("This is exactly what the broker will run, and what it will ask a\n")
	b.WriteString("human to approve. A command matching no rule is denied, so check\n")
	b.WriteString("here before trying something and finding out the hard way.\n\n")
	b.WriteString("```\n")
	printRules(&b, rules)
	b.WriteString("```\n")
	return os.WriteFile(filepath.Join(tmpDir, "CLAUDE.md"), []byte(b.String()), 0o644)
}

// bwaiModContent is a command-code mod (loaded with `cmd --mod`) whose
// only job is to append the bwai context to the system prompt. It reads
// the fragment from the read-only /run/bwai mount at call time, so the
// markdown stays the single source of truth shared with Claude Code.
//
// A mod plays the role Claude's `--add-dir /run/bwai` does: the agent
// opts in with a flag and bwai never writes to the agent's own config.
// command-code has no "additional memory directories" setting —
// subdirectory AGENTS.md only loads for files inside the project — so
// appending the prompt is the only way to inject this without shadowing
// the user's ~/.commandcode/AGENTS.md.
const bwaiModContent = `import type {ModApi} from '@commandcode/harness';
import {readFileSync} from 'node:fs';

// Exposed read-only by bwai whenever the broker is enabled.
const CONTEXT_PATH = '/run/bwai/CLAUDE.md';

export default function (cmd: ModApi): void {
	cmd.hooks({
		appendSystemPrompt: () => {
			try {
				return readFileSync(CONTEXT_PATH, 'utf8');
			} catch {
				return undefined;
			}
		},
	});
}
`

// installBwaiMod writes the command-code mod into the broker tmpdir.
// It's bind-mounted into the sandbox at /run/bwai/bwai.ts.
func installBwaiMod(tmpDir string) error {
	return os.WriteFile(filepath.Join(tmpDir, "bwai.ts"), []byte(bwaiModContent), 0o644)
}

// The config fragment passed to opencode via OPENCODE_CONFIG. The
// instructions field points at the read-only /run/bwai mount, keeping
// CLAUDE.md the single source of truth shared with Claude Code and
// command-code.
const opencodeConfigContent = `{
	"$schema": "https://opencode.ai/config.json",
	"instructions": ["/run/bwai/CLAUDE.md"]
}
`

// installOpencodeConfig writes the OPENCODE_CONFIG fragment into the
// broker tmpdir. bwai sets OPENCODE_CONFIG inside the sandbox, so
// opencode picks up the bwai context at startup without any flag, and
// without bwai writing to the agent's own config.
func installOpencodeConfig(tmpDir string) error {
	return os.WriteFile(filepath.Join(tmpDir, "opencode.json"), []byte(opencodeConfigContent), 0o644)
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
