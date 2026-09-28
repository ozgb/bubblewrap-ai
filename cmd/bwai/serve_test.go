package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBroker_RejectsForeignPeerUID(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled: true,
		Rules:   []Rule{{Match: []string{"true"}, Action: ActionAutoAllow}},
	}
	b := newTestBroker(t, cfg, projectDir)
	b.allowedUIDs = []int{os.Getuid() + 1}
	go b.Serve()
	frames := sendRequest(t, b.BrokerSocketPath(), brokerRequest{
		V: 1, Argv: []string{"true"}, Cwd: projectDir,
	})
	if len(frames) != 1 || frames[0].Type != frameTypeDenied || frames[0].Reason != denyReasonInvalid {
		t.Fatalf("frames = %+v, want a single invalid denial", frames)
	}
}

func TestBroker_AcceptsOwnPeerUID(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled: true,
		Rules:   []Rule{{Match: []string{"true"}, Action: ActionAutoAllow}},
	}
	b := newTestBroker(t, cfg, projectDir)
	b.allowedUIDs = []int{os.Getuid()}
	go b.Serve()
	frames := sendRequest(t, b.BrokerSocketPath(), brokerRequest{
		V: 1, Argv: []string{"true"}, Cwd: projectDir,
	})
	if _, _, code := collectStreams(t, frames); code == nil || *code != 0 {
		t.Fatalf("frames = %+v, want exit 0", frames)
	}
}

func TestCheckRateLimit_WindowForgetsOldConfirms(t *testing.T) {
	b := &Broker{confirmWindow: time.Hour}
	old := time.Now().Add(-2 * time.Hour)
	for i := 0; i < rateLimitConfirmsPerSess; i++ {
		b.confirmHist = append(b.confirmHist, old)
	}
	if !b.checkRateLimit() {
		t.Fatal("confirms older than the window must not count toward the cap")
	}

	session := &Broker{}
	for i := 0; i < rateLimitConfirmsPerSess; i++ {
		session.confirmHist = append(session.confirmHist, old)
	}
	if session.checkRateLimit() {
		t.Fatal("without a window the cap covers the broker's lifetime")
	}
}

func TestValidateServe(t *testing.T) {
	ok := ServeConfig{Socket: "/srv/b.sock", AllowedUIDs: []int{1001}, Roots: []string{"/srv/work"}}
	if err := validateServe(&ok); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := map[string]ServeConfig{
		"relative socket": {Socket: "b.sock", AllowedUIDs: []int{1}, Roots: []string{"/w"}},
		"no uids":         {Socket: "/b.sock", Roots: []string{"/w"}},
		"no roots":        {Socket: "/b.sock", AllowedUIDs: []int{1}},
		"relative root":   {Socket: "/b.sock", AllowedUIDs: []int{1}, Roots: []string{"w"}},
	}
	for name, c := range bad {
		if err := validateServe(&c); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := validateServe(nil); err == nil {
		t.Error("missing serve block: want an error")
	}
}

func TestNewServeBroker(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := t.TempDir()
	sock := filepath.Join(t.TempDir(), "broker.sock")
	cfg := BrokerConfig{
		Enabled: true,
		Rules:   []Rule{{Match: []string{"pwd"}, Action: ActionAutoAllow}},
		Serve:   &ServeConfig{Socket: sock, AllowedUIDs: []int{os.Getuid()}, Roots: []string{root}},
	}
	b, err := NewServeBroker(cfg, filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	go b.Serve()

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o666 {
		t.Errorf("socket mode = %v, want 0666 (the peer-uid check is the gate)", info.Mode().Perm())
	}

	frames := sendRequest(t, sock, brokerRequest{V: 1, Argv: []string{"pwd"}, Cwd: root})
	out, _, code := collectStreams(t, frames)
	if code == nil || *code != 0 {
		t.Fatalf("frames = %+v, want exit 0", frames)
	}
	if want := filepath.Join(serveRuntimeDir(), "cwd"); strings.TrimSpace(out) != want {
		t.Errorf("ran in %q, want %q", strings.TrimSpace(out), want)
	}

	frames = sendRequest(t, sock, brokerRequest{V: 1, Op: opContext})
	if len(frames) != 1 || !strings.Contains(frames[0].Data, "dedicated Unix user") ||
		strings.Contains(frames[0].Data, "inside a bwai sandbox") {
		t.Errorf("daemon context should describe a separate user, not a sandbox: %+v", frames)
	}

	frames = sendRequest(t, sock, brokerRequest{V: 1, Argv: []string{"pwd"}, Cwd: t.TempDir()})
	if len(frames) != 1 || frames[0].Type != frameTypeDenied {
		t.Errorf("cwd outside roots: frames = %+v, want a denial", frames)
	}

	if _, err := NewServeBroker(cfg, filepath.Join(t.TempDir(), "audit.log")); err == nil {
		t.Error("a second broker on a live socket must refuse to start")
	}
}

func TestBroker_ContextOp(t *testing.T) {
	cfg := BrokerConfig{
		Enabled: true,
		Rules:   []Rule{{Match: []string{"git-sign"}, Action: ActionAutoAllow}},
	}
	b := startTestBroker(t, cfg, t.TempDir())
	frames := sendRequest(t, b.BrokerSocketPath(), brokerRequest{V: 1, Op: opContext})
	if len(frames) != 1 || frames[0].Type != frameTypeContext {
		t.Fatalf("frames = %+v, want one context frame", frames)
	}
	if !strings.Contains(frames[0].Data, "# bwai broker") || !strings.Contains(frames[0].Data, "git-sign") {
		t.Errorf("context missing the guidance or the live rules:\n%s", frames[0].Data)
	}
}
