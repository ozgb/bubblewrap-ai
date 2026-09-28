package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

func main() {
	// Both bind-mounted helpers are the same binary under a different
	// argv[0]; name the client after whichever one was invoked so its
	// errors are not reported under the other's name.
	prog := filepath.Base(os.Args[0])
	if prog == "bwai-outside" || prog == "git-safe" || prog == "bwai-gpg" {
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
	// `bwai-gpg` is git's signing program inside the sandbox; it forwards
	// the buffer to the host's `bwai git-sign`.
	if prog == "bwai-gpg" {
		os.Exit(runGpgShim(os.Args[1:]))
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
		case "git-sign":
			os.Exit(runGitSign(os.Args[2:]))
		case "trust":
			os.Exit(runTrust(os.Args[2:]))
		}
	}
	os.Exit(runSandbox())
}

func runSandbox() int {
	versionFlag := flag.Bool("version", false, "Print version and exit")
	dumpConfig := flag.Bool("dump-config", false, "Print the default configuration JSON and exit")
	configFlag := flag.String("config", "", "Path to a config file (overrides ~/.config/bwai/bwai.json)")
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

	configPath, legacyConfig := resolveConfigPath(*configFlag, home)
	if legacyConfig {
		fmt.Fprintf(os.Stderr, "bwai: %s is deprecated; move it to %s\n", configPath, defaultConfigPath())
	}
	// A trusted .bwai.json in the sandbox root layers on top of the global
	// config: its list fields are appended, everything else overrides. Skip
	// it if it is the base itself (e.g. running from $HOME).
	localPath := filepath.Join(currentDir, ".bwai.json")
	if localPath == configPath {
		localPath = ""
	}
	cfg, localState, err := loadProjectConfig(configPath, localPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai: warning: could not load config: %v\n", err)
	}
	if localState == localUntrusted {
		fmt.Fprintln(os.Stderr, untrustedNotice(localPath))
	}
	homeAllow = cfg.HomeAllow
	homeBlock = cfg.HomeBlock

	// Persistent state root: created on the host and bound read-write below
	// so tool caches and installed binaries survive the session without
	// exposing the host's own caches. Omitted state_root defaults to the XDG
	// data dir; an explicit "" disables the mechanism.
	stateRoot := resolveStateRoot(cfg.StateRoot, home)
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
		// A linked worktree's git dirs are bound read-write too, and
		// git-safe has to read them to find the branch it pushes.
		roots = append(roots, gitWorktreeDirs(currentDir)...)
		broker, err = NewBroker(cfg.Broker, currentDir, defaultAuditPath(), roots...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwai: broker init failed: %v\n", err)
			return 1
		}
		if err := installBwaiOutsideHelper(broker.TmpDir()); err != nil {
			fmt.Fprintf(os.Stderr, "bwai: install helper: %v\n", err)
			_ = broker.Close()
			return 1
		}
		if err := installAgentMemoryFile(broker.TmpDir(), cfg.Broker.Rules, worktreeRoot, worktreeMainTree, opencodeAgentsPath(home), exposeMain); err != nil {
			fmt.Fprintf(os.Stderr, "bwai: install agent memory: %v\n", err)
			_ = broker.Close()
			return 1
		}
		if err := installBwaiMod(broker.TmpDir()); err != nil {
			fmt.Fprintf(os.Stderr, "bwai: install command-code mod: %v\n", err)
			_ = broker.Close()
			return 1
		}
		go broker.Serve()
		defer broker.Close()

		// The global config and trusted.json are read-only in the sandbox,
		// and a local file only counts once trusted, so the policy can
		// follow edits mid-session. Nothing is printed: the terminal belongs
		// to the agent's TUI.
		stopWatch := make(chan struct{})
		defer close(stopWatch)
		reload := sessionReloader(broker, configPath, localPath, localState == localApplied, func(bc BrokerConfig) {
			_ = installAgentMemoryFile(broker.TmpDir(), bc.Rules, worktreeRoot, worktreeMainTree, opencodeAgentsPath(home), exposeMain)
		})
		go watchConfig([]string{configPath, localPath, trustStorePath()}, configPollInterval, nil, stopWatch, reload,
			func(msg string, ok bool) {
				broker.auditLog.write(auditEntry{Decision: msg})
				if !ok {
					notifier("bwai: "+msg, currentDir)
				}
			})
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
		if !codexHomeWritable(codexHomePath(home, currentDir, cfg), home, currentDir, stateRoot, cfg) {
			fmt.Println("bwai: Codex broker guidance not mounted because CODEX_HOME is read-only; add it to home_allow or place it under the project/state_root.")
		}
		if !opencodeAgentsMountable(filepath.Dir(opencodeAgentsPath(home)), home, cfg) {
			fmt.Println("bwai: opencode broker guidance not mounted because ~/.config/opencode is missing or read-only; add it to home_allow.")
		}
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
			"--ro-bind", helper, "/run/bwai/bin/bwai-gpg",
			"--ro-bind", filepath.Join(broker.TmpDir(), "CLAUDE.md"), "/run/bwai/CLAUDE.md",
			"--ro-bind", filepath.Join(broker.TmpDir(), "bwai.ts"), "/run/bwai/bwai.ts",
			"--setenv", "BWAI_BROKER_SOCKET", "/run/bwai/broker.sock",
		)
		// opencode v2 accepts the config `instructions` field but never
		// resolves it; the only path that reaches the model is an AGENTS.md,
		// and the global one it reads is ~/.config/opencode/AGENTS.md. Overlay
		// a merged file (host instructions + broker fragment) so the host's
		// own config is neither written to nor shadowed.
		if opencodeAgents := opencodeAgentsPath(home); opencodeAgentsMountable(filepath.Dir(opencodeAgents), home, cfg) {
			args = append(args,
				"--ro-bind", filepath.Join(broker.TmpDir(), "OPENCODE_AGENTS.md"), opencodeAgents,
			)
		}
		args = append(args, sandboxGitConfigEnv()...)
		// Codex automatically loads AGENTS.md from CODEX_HOME. Overlay the
		// generated broker guidance there rather than replacing the project's
		// AGENTS.md or Codex's built-in instructions. Create the directory in
		// the sandbox when the host has no ~/.codex yet.
		codexHome := codexHomePath(home, currentDir, cfg)
		if codexHomeWritable(codexHome, home, currentDir, stateRoot, cfg) {
			args = append(args, missingBwrapDirs(codexHome)...)
			args = append(args,
				"--ro-bind", filepath.Join(broker.TmpDir(), "CODEX_AGENTS.md"), filepath.Join(codexHome, "AGENTS.md"),
			)
		}
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
	if broker != nil {
		// Push mirrors run git with host credentials, so their hooks and
		// config must be out of the agent's reach, whatever home_allow says.
		if priv := brokerPrivateDir(); os.MkdirAll(priv, 0o700) == nil {
			args = append(args, tmpfs(priv)...)
		}
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

// opencodeAgentsPath is the global AGENTS.md opencode v2 reads for
// instructions inside the sandbox. bwai does not pass XDG_CONFIG_HOME
// through by default, so opencode resolves the XDG default under HOME.
func opencodeAgentsPath(home string) string {
	return filepath.Join(home, ".config", "opencode", "AGENTS.md")
}

// opencodeAgentsMountable reports whether the sandbox can expose the merged
// AGENTS.md at dir/AGENTS.md. bwrap cannot create a mountpoint under a
// read-only parent, so the file must already exist or its directory must be
// writable in the sandbox (home_allow names it as a sub-path). A missing
// directory under the read-only home overlay cannot be created at all.
func opencodeAgentsMountable(dir, home string, cfg Config) bool {
	if info, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err == nil && !info.IsDir() {
		return true
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	rel, err := filepath.Rel(home, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	return slices.Contains(cfg.HomeAllow, filepath.ToSlash(rel))
}

// codexHomePath resolves the directory Codex will use in the sandbox. A
// CODEX_HOME from env_set takes precedence, followed by an explicitly
// allowed host value, then Codex's default under HOME.
func codexHomePath(home, currentDir string, cfg Config) string {
	value := cfg.EnvSet["CODEX_HOME"]
	if value == "" && slices.Contains(cfg.EnvAllow, "CODEX_HOME") {
		value = os.Getenv("CODEX_HOME")
	}
	if value == "" {
		return filepath.Join(home, ".codex")
	}
	value = expandHome(value, home)
	if !filepath.IsAbs(value) {
		value = filepath.Join(currentDir, value)
	}
	return filepath.Clean(value)
}

// codexHomeWritable reports whether the sandbox already has a writable
// mount covering CODEX_HOME, or the directory will be created on its
// writable home overlay. Respect an explicit home_allow restriction rather
// than making a previously read-only Codex directory writable for the broker.
func codexHomeWritable(codexHome, home, currentDir, stateRoot string, cfg Config) bool {
	_, statErr := os.Stat(codexHome)
	missing := os.IsNotExist(statErr)
	if isWithin(currentDir, codexHome) || (stateRoot != "" && isWithin(stateRoot, codexHome)) {
		return true
	}
	if !isWithin(home, codexHome) {
		return false
	}
	rel, err := filepath.Rel(home, codexHome)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	root := strings.Split(rel, string(os.PathSeparator))[0]
	return missing || matchesDirect(cfg.HomeAllow, root)
}

// missingBwrapDirs returns --dir arguments for missing path components,
// ordered from the shallowest missing parent to the requested directory.
func missingBwrapDirs(path string) []string {
	var missing []string
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			break
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
	}
	args := make([]string, 0, len(missing)*2)
	for i := len(missing) - 1; i >= 0; i-- {
		args = append(args, "--dir", missing[i])
	}
	return args
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

// defaultConfigPath is the global config location: $XDG_CONFIG_HOME/bwai/
// bwai.json, falling back to ~/.config/bwai/bwai.json.
func defaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "bwai", "bwai.json")
	}
	return filepath.Join(".config", "bwai", "bwai.json")
}

// resolveConfigPath picks the base config file. An explicit --config wins.
// Otherwise the XDG location is used when it exists, falling back to the
// legacy ~/.bwai.json so existing configs keep working. legacy reports that
// fallback, for a deprecation notice.
func resolveConfigPath(explicit, home string) (path string, legacy bool) {
	if explicit != "" {
		return explicit, false
	}
	xdg := defaultConfigPath()
	if _, err := os.Stat(xdg); err == nil {
		return xdg, false
	}
	old := filepath.Join(home, ".bwai.json")
	if _, err := os.Stat(old); err == nil {
		return old, true
	}
	return xdg, false
}

// defaultStateRoot is where persistent tool caches live when state_root is
// omitted: $XDG_DATA_HOME/bwai, falling back to ~/.local/share/bwai.
func defaultStateRoot(home string) string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "bwai")
	}
	return filepath.Join(home, ".local", "share", "bwai")
}

// resolveStateRoot picks the effective state root: the XDG data dir when
// state_root is omitted, "" when explicitly disabled, else the configured
// path with ~ expanded.
func resolveStateRoot(sr *string, home string) string {
	if sr == nil {
		return defaultStateRoot(home)
	}
	return expandHome(*sr, home)
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

{{intro}}

` + "`bwai-outside`" + ` is a narrow escape hatch for commands that need those
host credentials. **Default to running commands directly.** Only reach
for ` + "`bwai-outside`" + ` when a command would otherwise fail because a
credential it needs is deliberately out of reach. Ordinary work on local files —
including reading, editing, and committing — does *not* need it.

Use ` + "`bwai-outside`" + ` when the command requires host-only state:

` + "```sh" + `
bwai-outside gh pr create -R owner/repo --head my-branch --title "…" --body "…"
` + "```" + `

Host commands run in an empty directory on the host, never in your
project — a repository's hooks and config would otherwise run with the
host's credentials. So relative paths do not resolve, and ` + "`gh`" + ` cannot
infer the repository: always pass ` + "`-R owner/repo`" + ` (read it from
` + "`git remote -v`" + `), and ` + "`--head <branch>`" + ` for ` + "`gh pr create`" + `. An argument
naming an existing file outside the project is refused; give files by
absolute path inside the project, or pipe them with ` + "`--stdin`" + `:

` + "```sh" + `
bwai-outside --stdin gh issue create -R owner/repo -t "Title" -F - < body.md
` + "```" + `

Commits are signed on the host without any extra step: here, git's
signing program is ` + "`bwai-gpg`" + `, which hands the commit to the
broker to sign with the host's key. Just run ` + "`git commit`" + ` as usual.

Pushing has its own command, ` + "`git-safe`" + `. It is already on your ` + "`PATH`" + ` —
call it directly, with no ` + "`bwai-outside`" + ` prefix:

` + "```sh" + `
git-safe push [<remote>]      # publishes the current branch (fast-forward only)
` + "```" + `

` + "`bwai-outside git push`" + ` is deliberately not allowed. ` + "`git-safe push`" + `
pushes the current branch to the given remote (default ` + "`origin`" + `) and refuses
force, the protected branches (main/master/trunk/develop plus patterns from
` + "`broker.protected_branches`" + `), and any non-fast-forward update. The remote's
push URL must be listed in ` + "`broker.push_allowed_urls`" + `; an empty list allows
no push.

Run directly (do *not* prefix with ` + "`bwai-outside`" + `) for ordinary work —
these all succeed without it:

` + "```sh" + `
git status
git add -A
git commit -m "fix bug"          # signed through the broker
git diff
make test
npm install
` + "```" + `

Heuristic: if a command only touches the project tree or the network,
run it directly. ` + "`bwai-outside`" + ` is for the small set of operations
that need a credential you deliberately do not hold.

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
confirmed raw ` + "`git push`" + `. Taking the first rule that happens to
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

// installAgentMemoryFile writes the agent guidance into the broker
// tmpdir. It's bind-mounted into the sandbox at /run/bwai/CLAUDE.md,
// where Claude Code picks it up via `--add-dir /run/bwai`, and where the
// command-code mod below reads it. A copy is installed as ~/.codex/AGENTS.md
// inside the sandbox so Codex loads it as global instructions, and a merge
// of the host's global AGENTS.md with the fragment is installed for
// opencode at opencodeAgents.
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
func installAgentMemoryFile(tmpDir string, rules []Rule, worktreeRoot, mainTree, opencodeAgents string, exposeMain bool) error {
	content := []byte(agentContext(rules, worktreeRoot, mainTree, exposeMain))
	if err := os.WriteFile(filepath.Join(tmpDir, "CLAUDE.md"), content, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "CODEX_AGENTS.md"), content, 0o644); err != nil {
		return err
	}
	return installOpencodeAgents(tmpDir, opencodeAgents, content)
}

const sandboxIntro = `This shell runs inside a bwai sandbox. The project working tree is
read-write and the network is reachable, but host-only credentials —
` + "`~/.gnupg`" + `, ` + "`~/.ssh`" + `, ` + "`~/.aws`" + `, ` + "`gh`" + `'s auth, etc. — are deliberately
not mounted into the sandbox.`

const daemonIntro = `You run as a dedicated Unix user whose only job is agent work. You can
install and change anything you own — including with ` + "`sudo`" + ` inside your
container — and the network is reachable, but the credentials for signing,
pushing and GitHub belong to a different user and are deliberately out of
your reach. Work under ` + "`~/work`" + `: the broker refuses requests from anywhere
else.`

// agentContext renders the guidance for an agent in a bwai sandbox, with
// the live rule set.
func agentContext(rules []Rule, worktreeRoot, mainTree string, exposeMain bool) string {
	return renderAgentContext(sandboxIntro, rules, worktreeRoot, mainTree, exposeMain)
}

// daemonAgentContext is the guidance `bwai broker serve` hands out over
// `bwai-outside --context`, for agents running as a separate user.
func daemonAgentContext(rules []Rule) string {
	return renderAgentContext(daemonIntro, rules, "", "", false)
}

func renderAgentContext(intro string, rules []Rule, worktreeRoot, mainTree string, exposeMain bool) string {
	var b strings.Builder
	b.WriteString(strings.Replace(agentMemoryFileContent, "{{intro}}", intro, 1))
	if worktreeRoot != "" {
		b.WriteString(renderWorktreeSection(worktreeRoot, mainTree, exposeMain))
	}
	b.WriteString("\n## Broker rules\n\n")
	b.WriteString("This is exactly what the broker will run, and what it will ask a\n")
	b.WriteString("human to approve. A command matching no rule is denied, so check\n")
	b.WriteString("here before trying something and finding out the hard way.\n\n")
	b.WriteString("```\n")
	printRules(&b, rules)
	b.WriteString("```\n")
	return b.String()
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

// installOpencodeAgents writes the global AGENTS.md that opencode v2 reads
// for instructions. v2 accepts the config `instructions` field but never
// resolves it, so AGENTS.md is the only path that reaches the model. The
// host's own global instructions are preserved and the broker fragment
// appended; when the host file is already a bwai fragment (as
// bwai-refresh-context leaves it on an agent box) the fresh fragment
// replaces it rather than duplicating.
func installOpencodeAgents(tmpDir, hostAgents string, fragment []byte) error {
	merged := fragment
	if host, err := os.ReadFile(hostAgents); err == nil {
		if existing := strings.TrimSpace(string(host)); existing != "" && !strings.HasPrefix(existing, "# bwai broker") {
			merged = append(append([]byte(host), '\n', '\n'), fragment...)
		}
	}
	return os.WriteFile(filepath.Join(tmpDir, "OPENCODE_AGENTS.md"), merged, 0o644)
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
