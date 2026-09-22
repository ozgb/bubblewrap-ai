package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
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

	// Host-execution broker: lets specific argv lists escape the sandbox
	// with user approval. See docs/broker.md.
	Broker BrokerConfig `json:"broker"`
}

// BrokerConfig is the nested broker.* block. Disabled by default;
// `rules: []` means everything is denied.
type BrokerConfig struct {
	Enabled          bool      `json:"enabled"`
	Prompt           []string  `json:"prompt"`
	ApprovalTimeoutS int       `json:"approval_timeout_s"`
	Rules            []Rule    `json:"rules"`
	Web              WebConfig `json:"web"`
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
			// OpenRouter
			"OPENROUTER_API_KEY",
			// Command Code
			"COMMAND_CODE_API_KEY",
			"COMMANDCODE_API_URL",
			"CMD_LOCAL_ONLY",
		},
		HomeAllow: []string{
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
			".bash_history*",
			".config/Bitwarden",
			".cache/nvidia",
		},
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
// ~/.bwai.json, or --config), then layers the project-local config at
// localPath on top. Set-like list fields — home_allow, home_block and
// env_allow — are appended to the base, so a project only names what it
// adds. Everything else, including the argv lists command and
// bwrap_extra_args, overrides. An empty localPath skips the second layer.
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
// file are appended to cfg's instead of replacing them; argv lists such as
// bwrap_extra_args are left to replace, because appending them would reorder
// or duplicate flags.
func applyConfigFile(cfg *Config, path string, merge bool) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	// Snapshot the base lists before decoding: json.Unmarshal reuses a
	// slice's backing array, so appending afterwards would otherwise see the
	// local values already in place.
	var prevAllow, prevBlock, prevEnv []string
	if merge {
		prevAllow = append([]string(nil), cfg.HomeAllow...)
		prevBlock = append([]string(nil), cfg.HomeBlock...)
		prevEnv = append([]string(nil), cfg.EnvAllow...)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
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
	}
	return true, nil
}

func appendStrings(base, add []string) []string {
	out := make([]string, 0, len(base)+len(add))
	out = append(out, base...)
	return append(out, add...)
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
