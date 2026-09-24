package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWebApproveGate(t *testing.T) {
	cases := []struct {
		name   string
		prompt []string
		want   bool
	}{
		{"web present", []string{"web"}, true},
		{"web among others", []string{"oob", "web"}, true},
		{"oob only", []string{"oob"}, false},
		{"empty", []string{}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (BrokerConfig{Prompt: tc.prompt}).webApprove(); got != tc.want {
				t.Errorf("webApprove(%v) = %v, want %v", tc.prompt, got, tc.want)
			}
		})
	}
}

func TestValidateWebAddr(t *testing.T) {
	ok := []string{"127.0.0.1:0", "127.0.0.1:8080", "[::1]:0", "localhost:0"}
	for _, a := range ok {
		if err := validateWebAddr(a); err != nil {
			t.Errorf("validateWebAddr(%q) = %v, want nil", a, err)
		}
	}
	bad := []string{"0.0.0.0:8080", "192.168.1.5:80", ":8080", "8.8.8.8:53", "garbage"}
	for _, a := range bad {
		if err := validateWebAddr(a); err == nil {
			t.Errorf("validateWebAddr(%q) = nil, want error", a)
		}
	}
}

// TestDefaultConfigSupportsCommandCode pins the command-code wiring:
// ~/.commandcode must be writable in the sandbox (auth, sessions, taste,
// file history) and its env knobs must pass through.
func TestDefaultConfigSupportsCommandCode(t *testing.T) {
	cfg := defaultConfig()
	if !containsSequence(cfg.HomeAllow, ".commandcode") {
		t.Errorf("home_allow = %v, want it to include .commandcode", cfg.HomeAllow)
	}
	for _, key := range []string{"COMMAND_CODE_API_KEY", "COMMANDCODE_API_URL", "CMD_LOCAL_ONLY"} {
		if !containsSequence(cfg.EnvAllow, key) {
			t.Errorf("env_allow = %v, want it to include %s", cfg.EnvAllow, key)
		}
	}
}

// TestDefaultConfigIncludesGoBin pins that ~/go/bin — where `go install`
// drops binaries — is exposed. It lives outside a dotdir, so unlike
// ~/.cargo it is not even read-only mounted unless named explicitly.
func TestDefaultConfigIncludesGoBin(t *testing.T) {
	if cfg := defaultConfig(); !containsSequence(cfg.HomeAllow, "go/bin") {
		t.Errorf("home_allow = %v, want it to include go/bin", cfg.HomeAllow)
	}
}

// TestDefaultConfigBlocksRegistryCredentials pins that package-manager
// token files are hidden: they are the same class as ~/.ssh, and both npm
// and cargo keep registry API tokens in them.
func TestDefaultConfigBlocksRegistryCredentials(t *testing.T) {
	cfg := defaultConfig()
	for _, want := range []string{".npmrc", ".cargo/credentials.toml", ".cargo/credentials"} {
		if !containsSequence(cfg.HomeBlock, want) {
			t.Errorf("home_block = %v, want it to include %s", cfg.HomeBlock, want)
		}
	}
}

// TestLoadConfigRejectsNonLoopbackWebAddr pins the defence-in-depth gate:
// enabling web mode with a routable bind address must fail to load.
func TestLoadConfigRejectsNonLoopbackWebAddr(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bwai.json")
	cfgJSON := `{"broker":{"enabled":true,"prompt":["web"],"web":{"addr":"0.0.0.0:9000"}}}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("loadConfig accepted a non-loopback web.addr")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error = %v, want it to mention loopback", err)
	}
}

// TestLoadLayeredConfigMergesLists pins the project-local contract: set-like
// list fields (home_allow, env_allow, …) in the local file are appended to
// the base rather than replacing it, so a project only names what it adds.
// argv lists such as bwrap_extra_args and command override, because
// appending them would reorder or duplicate flags. Nested defaults are
// preserved when local omits them.
func TestLoadLayeredConfigMergesLists(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	local := filepath.Join(dir, "local.json")
	writeFile(t, base, `{"home_allow":["a"],"env_allow":["FOO"],"command":["bash"],"bwrap_extra_args":["--unshare-net"],"broker":{"approval_timeout_s":42}}`)
	writeFile(t, local, `{"home_allow":["b"],"env_allow":["BAR"],"command":["zsh"],"bwrap_extra_args":["--unshare-ipc"]}`)

	cfg, err := loadLayeredConfig(base, local)
	if err != nil {
		t.Fatalf("loadLayeredConfig: %v", err)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(cfg.HomeAllow, want) {
		t.Errorf("home_allow = %v, want %v", cfg.HomeAllow, want)
	}
	if want := []string{"FOO", "BAR"}; !reflect.DeepEqual(cfg.EnvAllow, want) {
		t.Errorf("env_allow = %v, want %v", cfg.EnvAllow, want)
	}
	if want := []string{"zsh"}; !reflect.DeepEqual(cfg.Command, want) {
		t.Errorf("command = %v, want %v (local overrides)", cfg.Command, want)
	}
	if want := []string{"--unshare-ipc"}; !reflect.DeepEqual(cfg.BwrapExtraArgs, want) {
		t.Errorf("bwrap_extra_args = %v, want %v (argv list must replace, not append)", cfg.BwrapExtraArgs, want)
	}
	if cfg.Broker.ApprovalTimeoutS != 42 {
		t.Errorf("broker.approval_timeout_s = %d, want 42 (local omits broker)", cfg.Broker.ApprovalTimeoutS)
	}
}

// TestLoadLayeredConfigMergesPathPrepend pins that path_prepend appends like
// the other set-like lists and env_set merges per key, so a project only
// names what it adds.
func TestLoadLayeredConfigMergesPathPrepend(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	local := filepath.Join(dir, "local.json")
	writeFile(t, base, `{"path_prepend":["/a"],"env_set":{"X":"1","Y":"2"}}`)
	writeFile(t, local, `{"path_prepend":["/b"],"env_set":{"Y":"3"}}`)

	cfg, err := loadLayeredConfig(base, local)
	if err != nil {
		t.Fatalf("loadLayeredConfig: %v", err)
	}
	if want := []string{"/a", "/b"}; !reflect.DeepEqual(cfg.PathPrepend, want) {
		t.Errorf("path_prepend = %v, want %v", cfg.PathPrepend, want)
	}
	if want := map[string]string{"X": "1", "Y": "3"}; !reflect.DeepEqual(cfg.EnvSet, want) {
		t.Errorf("env_set = %v, want %v (per-key merge)", cfg.EnvSet, want)
	}
}

func TestExpandHome(t *testing.T) {
	cases := []struct{ in, want string }{
		{"~", "/home/u"},
		{"~/x/y", "/home/u/x/y"},
		{"/abs/path", "/abs/path"},
		{"relative", "relative"},
		{"", ""},
		{"~user", "~user"}, // only a bare ~ or ~/ is expanded
	}
	for _, tc := range cases {
		if got := expandHome(tc.in, "/home/u"); got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestStateRootEnvArgs pins the bundle's shape: stable order, paths joined
// under the root, and CARGO_INSTALL_ROOT resolving to the root itself.
func TestStateRootEnvArgs(t *testing.T) {
	args := stateRootEnvArgs("/root")
	if !containsSequence(args, "--setenv", "CARGO_HOME", "/root/cargo") {
		t.Errorf("missing CARGO_HOME; args: %v", args)
	}
	if !containsSequence(args, "--setenv", "UV_TOOL_BIN_DIR", "/root/bin") {
		t.Errorf("missing UV_TOOL_BIN_DIR; args: %v", args)
	}
	if !containsSequence(args, "--setenv", "CARGO_INSTALL_ROOT", "/root") {
		t.Errorf("CARGO_INSTALL_ROOT should resolve to the root itself; args: %v", args)
	}
	if !containsSequence(args, "--setenv", "GOMODCACHE", "/root/go/pkg/mod") {
		t.Errorf("missing GOMODCACHE; args: %v", args)
	}
	if got, want := len(args), 3*len(stateRootEnv); got != want {
		t.Errorf("len(args) = %d, want %d (3 per var)", got, want)
	}
}

// TestLoadLayeredConfigAbsentLocal pins that a missing local file leaves the
// base untouched, and that omitting a list locally does not duplicate it.
func TestLoadLayeredConfigAbsentLocal(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	writeFile(t, base, `{"home_allow":["a"]}`)

	cfg, err := loadLayeredConfig(base, filepath.Join(dir, "missing.json"))
	if err != nil {
		t.Fatalf("loadLayeredConfig: %v", err)
	}
	if want := []string{"a"}; !reflect.DeepEqual(cfg.HomeAllow, want) {
		t.Errorf("home_allow = %v, want %v", cfg.HomeAllow, want)
	}

	local := filepath.Join(dir, "local.json")
	writeFile(t, local, `{"command":["zsh"]}`)
	cfg, err = loadLayeredConfig(base, local)
	if err != nil {
		t.Fatalf("loadLayeredConfig: %v", err)
	}
	if want := []string{"a"}; !reflect.DeepEqual(cfg.HomeAllow, want) {
		t.Errorf("home_allow = %v, want %v (omitted list must not duplicate)", cfg.HomeAllow, want)
	}
}

// TestLoadLayeredConfigValidatesLocal pins that validation runs over the
// merged result, not just the base file.
func TestLoadLayeredConfigValidatesLocal(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.json")
	local := filepath.Join(dir, "local.json")
	writeFile(t, base, `{}`)
	writeFile(t, local, `{"broker":{"enabled":true,"prompt":["web"],"web":{"addr":"0.0.0.0:9000"}}}`)

	if _, err := loadLayeredConfig(base, local); err == nil {
		t.Fatal("loadLayeredConfig accepted a non-loopback web.addr from the local config")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadConfigWebDefaults verifies web mode without an explicit addr
// inherits the ephemeral-loopback default rather than binding the wildcard.
func TestLoadConfigWebDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bwai.json")
	cfgJSON := `{"broker":{"enabled":true,"prompt":["web"]}}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Broker.Web.Addr != defaultWebAddr {
		t.Errorf("web.addr = %q, want default %q", cfg.Broker.Web.Addr, defaultWebAddr)
	}
}
