package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

type Config struct {
	// Path to the bwrap binary. Defaults to "bwrap"
	BwrapPath string `json:"bwrap_path"`

	// Extra arguments passed to bwrap. Use this to add --unshare-net, --setenv HTTP_PROXY, etc...
	BwrapExtraArgs []string `json:"bwrap_extra_args"`

	// Default command to run. Defaults to ["bash"]
	Command []string `json:"command"`

	// Files and directories in $HOME that agents need write access to
	HomeAllow []string `json:"home_allow"`

	// Sensitive files and directories in $HOME that must never be exposed
	HomeBlock []string `json:"home_block"`

	// Environment variables from the host that are passed into the sandbox
	EnvAllow []string `json:"env_allow"`

	// StateRoot is a host directory bwai creates and mounts read-write as a
	// persistent home for tool caches and installed binaries. When set, bwai
	// points the package managers it knows (npm, cargo, uv, go, pip, ...) at
	// it and prepends <state_root>/bin to PATH, so the sandbox's caches stay
	// warm across sessions while the host's own caches stay untouched.
	// Omitted, it defaults to $XDG_DATA_HOME/bwai (~/.local/share/bwai); set
	// it to "" to disable the mechanism.
	StateRoot *string `json:"state_root"`

	// EnvSet sets literal environment variables in the sandbox; values are
	// ~-expanded. Applied after the state-root bundle, so it can override it.
	// When config layers, entries merge per key rather than replacing.
	EnvSet map[string]string `json:"env_set"`

	// PathPrepend lists directories prepended to the sandbox PATH, in order
	// and ahead of <state_root>/bin. ~ expanded.
	PathPrepend []string `json:"path_prepend"`

	// Worktrees exposes the repo's persistent worktree root when bwai is
	// started inside a linked worktree, so `wt` worktrees survive the
	// session (see worktreeSiblingMounts). Defaults to true.
	Worktrees *WorktreesConfig `json:"worktrees"`

	// Host-execution broker: lets specific argv lists escape the sandbox
	// with user approval. See docs/broker.md.
	Broker BrokerConfig `json:"broker"`
}

// WorktreesConfig tunes worktree exposure. ExposeMain controls whether
// the main checkout's working tree is mounted read-write when bwai
// starts in a linked worktree — on by default, off for the paranoid
// (another agent may hold the main tree, or you may simply not trust
// writes outside the sandbox root).
type WorktreesConfig struct {
	ExposeMain bool `json:"expose_main"`
}

// BrokerConfig is the nested broker.* block. Disabled by default;
// `rules: []` means everything is denied.
type BrokerConfig struct {
	Enabled          bool      `json:"enabled"`
	Prompt           []string  `json:"prompt"`
	ApprovalTimeoutS int       `json:"approval_timeout_s"`
	Rules            []Rule    `json:"rules"`
	Web              WebConfig `json:"web"`

	// PushAllowedURLs is the set of remote URLs `git-safe push` may target,
	// compared after normalisation. It is a trust anchor rather than an
	// ordinary setting: the broker injects its own copy into the host-side
	// push, and a project-local .bwai.json only reaches that copy once it is
	// trusted, so an edit in the project tree cannot widen it. Empty means
	// every push is refused.
	PushAllowedURLs []string `json:"push_allowed_urls"`

	// ProtectedBranches adds branch-name patterns `git-safe push` refuses,
	// on top of the built-in main/master/trunk/develop. An entry is either an
	// exact name or a shell glob, so "release-*" or "release/*" covers a
	// family. The branch being pushed is the destination branch, since the
	// refspec is always HEAD:refs/heads/<branch>. Like PushAllowedURLs it is
	// a trust anchor, injected into the push from the broker's own config.
	ProtectedBranches []string `json:"protected_branches"`

	// Serve configures `bwai broker serve`, the long-running broker for
	// agents that run as another user rather than inside a bwai sandbox.
	// Only read from the global config.
	Serve *ServeConfig `json:"serve,omitempty"`
}

// ServeConfig is the daemon's socket and the agents it answers.
type ServeConfig struct {
	// Socket is the broker socket path. It has to be the same path on the
	// host and wherever the agent runs, and its directory must let the
	// agent users reach it.
	Socket string `json:"socket"`
	// AllowedUIDs are the uids that may connect, checked with SO_PEERCRED.
	AllowedUIDs []int `json:"allowed_uids"`
	// Roots are the directories requests may come from — the agents' work
	// trees. Paths must match between the host and the agent's view.
	Roots []string `json:"roots"`
}

// WebConfig configures the loopback HTTP approval page used by the
// "web" prompt mode. Addr is the bind address; it must resolve to a
// loopback host (validated in loadConfig) so the page is never exposed
// beyond this machine. The default ":0" picks an ephemeral port, one
// per bwai instance — mirroring the per-PID tmpdir multi-instance story.
type WebConfig struct {
	Addr string `json:"addr"`
}

// defaultWebAddr binds the approval page to an ephemeral loopback port.
const defaultWebAddr = "127.0.0.1:0"

func defaultConfig() Config {
	return Config{
		BwrapPath:      "bwrap",
		BwrapExtraArgs: []string{"--unshare-pid", "--unshare-ipc"},
		Command:        []string{"bash"},
		Worktrees:      &WorktreesConfig{ExposeMain: true},
		EnvAllow: []string{
			"TERM",
			"COLORTERM",
			"LANG",
			"LC_ALL",
			"LC_MESSAGES",
			"LC_CTYPE",
			"HOME",
			"USER",
			"LOGNAME",
			"PATH",
			"EDITOR",
			// Claude
			"ANTHROPIC_API_KEY",
			// Claude model selection / pinning
			"ANTHROPIC_MODEL",
			"ANTHROPIC_DEFAULT_OPUS_MODEL",
			"ANTHROPIC_DEFAULT_SONNET_MODEL",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL",
			// Claude Code on Google Vertex AI
			"CLAUDE_CODE_USE_VERTEX",
			"CLOUD_ML_REGION",
			"ANTHROPIC_VERTEX_PROJECT_ID",
			// Gemini / Google
			"GEMINI_API_KEY",
			"GOOGLE_API_KEY",
			"GCLOUD_PROJECT",
			"GOOGLE_CLOUD_PROJECT",
			// Goose (uses provider keys above + its own config)
			"GOOSE_PROVIDER",
			"GOOSE_MODEL",
			"GOOSE_PLANNER_PROVIDER",
			"GOOSE_PLANNER_MODEL",
			// OpenAI-compatible providers (used by Goose and others)
			"OPENAI_API_KEY",
			"OPENAI_API_BASE",
			// Codex CLI state directory override
			"CODEX_HOME",
			// OpenRouter
			"OPENROUTER_API_KEY",
			// Command Code
			"COMMAND_CODE_API_KEY",
			"COMMANDCODE_API_URL",
			"CMD_LOCAL_ONLY",
		},
		HomeAllow: []string{
			".codex", // Codex CLI config, login state, and sessions
			".claude",
			".gemini",
			".claude.json",
			".config/goose",
			".config/opencode",      // opencode config
			".local/share/opencode", // opencode auth, sessions, state
			".config/gcloud",
			".local/state",
			".local/share/goose",
			".cache",
			".cargo",
			"go/bin",       // go-installed tools; ~/go is not a dotdir, so it'd otherwise be hidden
			".commandcode", // Command Code
		},
		HomeBlock: []string{
			".gnupg",
			".ssh",
			".pki",
			".aws",
			".kube",
			".azure",
			".bashrc",
			".bashrc.d",
			".password-store",
			// Registry API tokens, the same class as .ssh/.aws. .cargo is
			// otherwise writable, so only its credential files are masked.
			".npmrc",
			".cargo/credentials.toml",
			".cargo/credentials",
			".bash_history*",
			".config/Bitwarden",
			".cache/nvidia",
		},
		EnvSet:      map[string]string{},
		PathPrepend: []string{},
		Broker: BrokerConfig{
			Enabled:          false,
			Prompt:           []string{"oob"},
			ApprovalTimeoutS: defaultApprovalTimeoutSec,
			Rules:            []Rule{},
			Web:              WebConfig{Addr: defaultWebAddr},
		},
	}
}

// loadConfig reads the global config at path if it exists and returns it on
// top of the defaults. Fields omitted from the file fall back to the defaults.
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	if _, err := applyConfigFile(&cfg, path, false); err != nil {
		return cfg, err
	}
	if err := validateConfig(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// loadLayeredConfig loads the base config at basePath (the global
// ~/.config/bwai/bwai.json, or --config), then layers the project-local
// config at localPath on top. Set-like fields — home_allow, home_block, env_allow,
// path_prepend and the env_set map — are added to the base (lists append,
// env_set merges per key), so a project only names what it adds. Everything
// else, including the argv lists command and bwrap_extra_args, overrides. An
// empty localPath skips the second layer.
// Missing files are not an error.
func loadLayeredConfig(basePath, localPath string) (Config, error) {
	cfg := defaultConfig()
	if _, err := applyConfigFile(&cfg, basePath, false); err != nil {
		return cfg, err
	}
	if localPath != "" {
		if _, err := applyConfigFile(&cfg, localPath, true); err != nil {
			return cfg, err
		}
	}
	if err := validateConfig(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyConfigFile decodes the JSON file at path onto cfg and reports whether
// the file existed. Decoding onto the existing value (rather than a fresh
// struct) is deliberate: omitted nested fields such as broker.web.addr keep
// their defaults. When merge is true, set-like list fields present in the
// file are appended to cfg's instead of replacing them, including
// broker.rules (base first, so the base keeps first-match precedence); argv
// lists such as bwrap_extra_args are left to replace, because appending them
// would reorder or duplicate flags.
func applyConfigFile(cfg *Config, path string, merge bool) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, applyConfigData(cfg, data, path, merge)
}

// applyConfigData is applyConfigFile on bytes already read, so a caller
// that has checked those exact bytes applies the same ones.
func applyConfigData(cfg *Config, data []byte, path string, merge bool) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// Snapshot the base lists before decoding: json.Unmarshal reuses a
	// slice's backing array, so appending afterwards would otherwise see the
	// local values already in place.
	var prevAllow, prevBlock, prevEnv, prevPath []string
	var prevRules []Rule
	if merge {
		prevAllow = append([]string(nil), cfg.HomeAllow...)
		prevBlock = append([]string(nil), cfg.HomeBlock...)
		prevEnv = append([]string(nil), cfg.EnvAllow...)
		prevPath = append([]string(nil), cfg.PathPrepend...)
		prevRules = cloneRules(cfg.Broker.Rules)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if merge {
		if _, ok := keys["home_allow"]; ok {
			cfg.HomeAllow = appendStrings(prevAllow, cfg.HomeAllow)
		}
		if _, ok := keys["home_block"]; ok {
			cfg.HomeBlock = appendStrings(prevBlock, cfg.HomeBlock)
		}
		if _, ok := keys["env_allow"]; ok {
			cfg.EnvAllow = appendStrings(prevEnv, cfg.EnvAllow)
		}
		if _, ok := keys["path_prepend"]; ok {
			cfg.PathPrepend = appendStrings(prevPath, cfg.PathPrepend)
		}
		// broker.rules merges like the other lists, base first. Matching is
		// first-match, so the base rules keep precedence and a project adds
		// to the set rather than replacing it.
		if brokerKeyPresent(keys, "rules") {
			cfg.Broker.Rules = append(prevRules, cfg.Broker.Rules...)
		}
	}
	return nil
}

// brokerKeyPresent reports whether the top-level "broker" object names key.
// Used to tell "local omitted the field" from "local set it", which decides
// whether a merge appends the base instead of silently duplicating it.
func brokerKeyPresent(top map[string]json.RawMessage, key string) bool {
	raw, ok := top["broker"]
	if !ok {
		return false
	}
	var broker map[string]json.RawMessage
	if err := json.Unmarshal(raw, &broker); err != nil {
		return false
	}
	_, ok = broker[key]
	return ok
}

func appendStrings(base, add []string) []string {
	out := make([]string, 0, len(base)+len(add))
	out = append(out, base...)
	return append(out, add...)
}

// cloneRules deep-copies a rule list, including each Match slice. A shallow
// copy is not enough: json.Unmarshal reuses the backing arrays of existing
// slices, so decoding the local rules would overwrite the base rules' Match
// tokens through the shared array.
func cloneRules(rules []Rule) []Rule {
	out := make([]Rule, len(rules))
	for i, r := range rules {
		out[i] = Rule{
			Match:  append([]string(nil), r.Match...),
			Action: r.Action,
		}
	}
	return out
}

func validateServe(s *ServeConfig) error {
	if s == nil {
		return errors.New("broker.serve is not configured")
	}
	if !filepath.IsAbs(s.Socket) {
		return fmt.Errorf("broker.serve.socket %q must be an absolute path", s.Socket)
	}
	if len(s.AllowedUIDs) == 0 {
		return errors.New("broker.serve.allowed_uids must name at least one uid")
	}
	if len(s.Roots) == 0 {
		return errors.New("broker.serve.roots must name at least one directory")
	}
	for _, r := range s.Roots {
		if !filepath.IsAbs(r) {
			return fmt.Errorf("broker.serve.roots entry %q must be an absolute path", r)
		}
	}
	return nil
}

func validateConfig(cfg *Config) error {
	for i, r := range cfg.Broker.Rules {
		if err := validateRule(r); err != nil {
			return fmt.Errorf("broker.rules[%d]: %w", i, err)
		}
	}
	// The web approval page is reachable from the sandbox (it shares the
	// host network namespace), so the token is the only authorizer.
	// Refuse to even start if the bind address isn't loopback — defence
	// in depth against accidentally exposing approvals to the LAN.
	if cfg.Broker.Enabled && cfg.Broker.webApprove() {
		if cfg.Broker.Web.Addr == "" {
			cfg.Broker.Web.Addr = defaultWebAddr
		}
		if err := validateWebAddr(cfg.Broker.Web.Addr); err != nil {
			return fmt.Errorf("broker.web.addr: %w", err)
		}
	}
	return nil
}

// validateWebAddr rejects any bind address that does not resolve to a
// loopback host. "localhost" is accepted; bare ports, wildcard hosts,
// and routable IPs are not.
func validateWebAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not a valid host:port: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("%q must name a loopback host (e.g. 127.0.0.1)", addr)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("host %q is not a loopback address", host)
	}
	return nil
}

// webApprove reports whether the loopback web approval page is enabled —
// i.e. "web" is in the configured prompt stack. Off by default; opt in
// by adding "web" to broker.prompt.
func (c BrokerConfig) webApprove() bool {
	for _, m := range c.Prompt {
		if m == "web" {
			return true
		}
	}
	return false
}

// Package-level vars set in main()
var homeAllow []string
var homeBlock []string
