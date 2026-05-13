package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startTestBroker spins up a broker against a per-test tmpdir and
// returns it. Closes itself via t.Cleanup. We override the on-disk
// layout by setting the broker's tmp dir to a per-test path; the
// production path uses /tmp/bwai-$PID, but tests need isolation.
func startTestBroker(t *testing.T, cfg BrokerConfig, projectDir string) *Broker {
	t.Helper()
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "bin"), 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	auditPath := filepath.Join(tmpDir, "broker.log")

	brokerSock := filepath.Join(tmpDir, "broker.sock")
	approveSock := filepath.Join(tmpDir, "approve.sock")
	brokerLn, err := net.Listen("unix", brokerSock)
	if err != nil {
		t.Fatalf("listen broker: %v", err)
	}
	approveLn, err := net.Listen("unix", approveSock)
	if err != nil {
		_ = brokerLn.Close()
		t.Fatalf("listen approve: %v", err)
	}
	audit, err := newAuditLogger(auditPath)
	if err != nil {
		_ = brokerLn.Close()
		_ = approveLn.Close()
		t.Fatalf("audit: %v", err)
	}
	if cfg.ApprovalTimeoutS <= 0 {
		cfg.ApprovalTimeoutS = defaultApprovalTimeoutSec
	}
	b := &Broker{
		cfg:        cfg,
		projectDir: projectDir,
		auditLog:   audit,
		brokerLn:   brokerLn,
		approveLn:  approveLn,
		tmpDir:     tmpDir,
		pending:    map[string]*pendingRequest{},
	}
	go b.Serve()
	t.Cleanup(func() {
		_ = b.Close()
	})
	return b
}

func sendRequest(t *testing.T, sockPath string, req brokerRequest) []brokerFrame {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode req: %v", err)
	}
	var frames []brokerFrame
	dec := json.NewDecoder(conn)
	for {
		var fr brokerFrame
		if err := dec.Decode(&fr); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode frame: %v", err)
		}
		frames = append(frames, fr)
	}
	return frames
}

func TestBroker_AutoAllow(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled: true,
		Rules: []Rule{
			{Match: []string{"echo", "hello"}, Action: ActionAutoAllow},
		},
	}
	b := startTestBroker(t, cfg, projectDir)
	frames := sendRequest(t, b.BrokerSocketPath(), brokerRequest{
		V: 1, Argv: []string{"echo", "hello"}, Cwd: projectDir,
	})

	// Expect: stdout("hello\n") + exit 0. No pending frame, no denied.
	var sawStdout bool
	var exitCode *int
	for _, fr := range frames {
		switch fr.Type {
		case frameTypeStdout:
			if fr.Data != "hello\n" {
				t.Errorf("stdout %q want %q", fr.Data, "hello\n")
			}
			sawStdout = true
		case frameTypeExit:
			exitCode = fr.Code
		case frameTypePending, frameTypeDenied:
			t.Errorf("unexpected frame: %+v", fr)
		}
	}
	if !sawStdout {
		t.Error("missing stdout frame")
	}
	if exitCode == nil || *exitCode != 0 {
		t.Errorf("exit code = %v, want 0", exitCode)
	}
}

func TestBroker_AutoDenyImplicit(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{Enabled: true} // empty rules → implicit deny
	b := startTestBroker(t, cfg, projectDir)
	frames := sendRequest(t, b.BrokerSocketPath(), brokerRequest{
		V: 1, Argv: []string{"echo", "no"}, Cwd: projectDir,
	})
	if len(frames) != 1 || frames[0].Type != frameTypeDenied || frames[0].Reason != denyReasonRule {
		t.Fatalf("expected single deny:rule frame, got %+v", frames)
	}
}

func TestBroker_RejectsCwdOutsideProject(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled: true,
		Rules:   []Rule{{Match: []string{"echo", "x"}, Action: ActionAutoAllow}},
	}
	b := startTestBroker(t, cfg, projectDir)
	frames := sendRequest(t, b.BrokerSocketPath(), brokerRequest{
		V: 1, Argv: []string{"echo", "x"}, Cwd: "/etc",
	})
	if len(frames) != 1 || frames[0].Type != frameTypeDenied || frames[0].Reason != denyReasonInvalid {
		t.Fatalf("expected deny:invalid for out-of-project cwd, got %+v", frames)
	}
}

func TestBroker_ConfirmApproved(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled:          true,
		ApprovalTimeoutS: 5,
		Rules: []Rule{
			{Match: []string{"echo", "approved"}, Action: ActionConfirm},
		},
	}
	b := startTestBroker(t, cfg, projectDir)

	// Send request in background; main goroutine plays approver.
	type result struct {
		frames []brokerFrame
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		frames := sendRequest(t, b.BrokerSocketPath(), brokerRequest{
			V: 1, Argv: []string{"echo", "approved"}, Cwd: projectDir,
		})
		resCh <- result{frames: frames}
	}()

	// Poll approve.sock until the request shows up, then approve it.
	deadline := time.Now().Add(2 * time.Second)
	var approvedID string
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", b.ApproveSocketPath())
		if err != nil {
			t.Fatalf("dial approve: %v", err)
		}
		_ = json.NewEncoder(conn).Encode(approverRequest{Op: "list"})
		var reply approverReply
		if err := json.NewDecoder(conn).Decode(&reply); err != nil {
			conn.Close()
			t.Fatalf("decode list: %v", err)
		}
		if len(reply.Pending) == 1 {
			approvedID = reply.Pending[0].ID
			_ = json.NewEncoder(conn).Encode(approverRequest{Op: "decide", ID: approvedID, Decision: "approve"})
			var dreply approverReply
			if err := json.NewDecoder(conn).Decode(&dreply); err != nil {
				conn.Close()
				t.Fatalf("decode decide: %v", err)
			}
			if !dreply.OK {
				conn.Close()
				t.Fatalf("decide not OK: %s", dreply.Error)
			}
			conn.Close()
			break
		}
		conn.Close()
		time.Sleep(20 * time.Millisecond)
	}
	if approvedID == "" {
		t.Fatal("never saw pending request on approve.sock")
	}

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("client err: %v", res.err)
		}
		var sawPending, sawStdout bool
		var exitCode *int
		for _, fr := range res.frames {
			switch fr.Type {
			case frameTypePending:
				sawPending = true
				if fr.ID != approvedID {
					t.Errorf("pending id %q want %q", fr.ID, approvedID)
				}
			case frameTypeStdout:
				if strings.TrimSpace(fr.Data) != "approved" {
					t.Errorf("stdout %q", fr.Data)
				}
				sawStdout = true
			case frameTypeExit:
				exitCode = fr.Code
			}
		}
		if !sawPending {
			t.Error("missing pending frame")
		}
		if !sawStdout {
			t.Error("missing stdout frame")
		}
		if exitCode == nil || *exitCode != 0 {
			t.Errorf("exit = %v want 0", exitCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client never returned")
	}
}

func TestBroker_ConfirmDenied(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled:          true,
		ApprovalTimeoutS: 5,
		Rules:            []Rule{{Match: []string{"echo", "denyme"}, Action: ActionConfirm}},
	}
	b := startTestBroker(t, cfg, projectDir)

	resCh := make(chan []brokerFrame, 1)
	go func() {
		resCh <- sendRequest(t, b.BrokerSocketPath(), brokerRequest{
			V: 1, Argv: []string{"echo", "denyme"}, Cwd: projectDir,
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", b.ApproveSocketPath())
		if err != nil {
			t.Fatalf("dial approve: %v", err)
		}
		_ = json.NewEncoder(conn).Encode(approverRequest{Op: "list"})
		var reply approverReply
		_ = json.NewDecoder(conn).Decode(&reply)
		if len(reply.Pending) == 1 {
			_ = json.NewEncoder(conn).Encode(approverRequest{Op: "decide", ID: reply.Pending[0].ID, Decision: "deny"})
			var dreply approverReply
			_ = json.NewDecoder(conn).Decode(&dreply)
			conn.Close()
			break
		}
		conn.Close()
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case frames := <-resCh:
		var sawDenied bool
		for _, fr := range frames {
			if fr.Type == frameTypeDenied && fr.Reason == denyReasonUser {
				sawDenied = true
			}
		}
		if !sawDenied {
			t.Fatalf("expected deny:user, got %+v", frames)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client never returned")
	}
}

func TestBroker_AlwaysThisSessionPromotesToAutoAllow(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled:          true,
		ApprovalTimeoutS: 5,
		Rules:            []Rule{{Match: []string{"echo", "twice"}, Action: ActionConfirm}},
	}
	b := startTestBroker(t, cfg, projectDir)

	// First request: approve with "always".
	resCh := make(chan []brokerFrame, 1)
	go func() {
		resCh <- sendRequest(t, b.BrokerSocketPath(), brokerRequest{
			V: 1, Argv: []string{"echo", "twice"}, Cwd: projectDir,
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", b.ApproveSocketPath())
		if err != nil {
			t.Fatalf("dial approve: %v", err)
		}
		_ = json.NewEncoder(conn).Encode(approverRequest{Op: "list"})
		var reply approverReply
		_ = json.NewDecoder(conn).Decode(&reply)
		if len(reply.Pending) == 1 {
			_ = json.NewEncoder(conn).Encode(approverRequest{Op: "decide", ID: reply.Pending[0].ID, Decision: "always"})
			var dreply approverReply
			_ = json.NewDecoder(conn).Decode(&dreply)
			conn.Close()
			break
		}
		conn.Close()
		time.Sleep(20 * time.Millisecond)
	}
	<-resCh

	// Second request: same argv. Should auto_allow without a pending frame.
	frames := sendRequest(t, b.BrokerSocketPath(), brokerRequest{
		V: 1, Argv: []string{"echo", "twice"}, Cwd: projectDir,
	})
	for _, fr := range frames {
		if fr.Type == frameTypePending {
			t.Fatalf("second request should not have prompted: %+v", frames)
		}
	}
}

func TestBroker_ConfirmTimesOut(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled:          true,
		ApprovalTimeoutS: 1, // shortest the protocol allows in seconds
		Rules:            []Rule{{Match: []string{"echo", "slow"}, Action: ActionConfirm}},
	}
	b := startTestBroker(t, cfg, projectDir)

	frames := sendRequest(t, b.BrokerSocketPath(), brokerRequest{
		V: 1, Argv: []string{"echo", "slow"}, Cwd: projectDir,
	})
	// Expected: pending frame, then denied:timeout. No exec.
	if len(frames) < 2 {
		t.Fatalf("expected pending+denied, got %+v", frames)
	}
	if frames[0].Type != frameTypePending {
		t.Errorf("first frame = %+v, want pending", frames[0])
	}
	last := frames[len(frames)-1]
	if last.Type != frameTypeDenied || last.Reason != denyReasonTimeout {
		t.Errorf("last frame = %+v, want denied:timeout", last)
	}
}

func TestBroker_RejectsInvalidVersion(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled: true,
		Rules:   []Rule{{Match: []string{"echo", "ok"}, Action: ActionAutoAllow}},
	}
	b := startTestBroker(t, cfg, projectDir)
	frames := sendRequest(t, b.BrokerSocketPath(), brokerRequest{
		V: 99, Argv: []string{"echo", "ok"}, Cwd: projectDir,
	})
	if len(frames) != 1 || frames[0].Type != frameTypeDenied || frames[0].Reason != denyReasonInvalid {
		t.Fatalf("expected deny:invalid, got %+v", frames)
	}
}
