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
		},
		HomeAllow: []string{
			".claude",
			".gemini",
			".claude.json",
			".config/goose",
			".config/gcloud",
			".local/state",
			".local/share/goose",
			".cache",
			".cargo",
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

// loadConfig reads the config file at the given path if it exists and returns the resulting Config.
// Fields omitted from the file fall back to the defaults.
func loadConfig(path string) (cfg Config, err error) {
	cfg = defaultConfig()
	var f *os.File
	f, err = os.Open(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if err = json.NewDecoder(f).Decode(&cfg); err != nil {
		return cfg, err
	}
	for i, r := range cfg.Broker.Rules {
		if rerr := validateRule(r); rerr != nil {
			return cfg, fmt.Errorf("broker.rules[%d]: %w", i, rerr)
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
		if verr := validateWebAddr(cfg.Broker.Web.Addr); verr != nil {
			return cfg, fmt.Errorf("broker.web.addr: %w", verr)
		}
	}
	return cfg, nil
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
