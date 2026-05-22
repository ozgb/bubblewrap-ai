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

	// Expect: stdout("hello\n") + exit 0. Streaming may split output
	// across multiple frames, so concatenate before comparing.
	gotStdout, _, exitCode := collectStreams(t, frames)
	if gotStdout != "hello\n" {
		t.Errorf("stdout = %q, want %q", gotStdout, "hello\n")
	}
	for _, fr := range frames {
		if fr.Type == frameTypePending || fr.Type == frameTypeDenied {
			t.Errorf("unexpected frame: %+v", fr)
		}
	}
	if exitCode == nil || *exitCode != 0 {
		t.Errorf("exit code = %v, want 0", exitCode)
	}
}

// collectStreams concatenates stdout/stderr frames in arrival order and
// returns the final exit code. Streaming means a single conceptual
// "stdout" can arrive as several frames; tests should never assume a
// fixed frame count.
func collectStreams(t *testing.T, frames []brokerFrame) (stdout, stderr string, exit *int) {
	t.Helper()
	var sb, eb strings.Builder
	for _, fr := range frames {
		switch fr.Type {
		case frameTypeStdout:
			sb.WriteString(fr.Data)
		case frameTypeStderr:
			eb.WriteString(fr.Data)
		case frameTypeExit:
			exit = fr.Code
		}
	}
	return sb.String(), eb.String(), exit
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
		var sawPending bool
		for _, fr := range res.frames {
			if fr.Type == frameTypePending {
				sawPending = true
				if fr.ID != approvedID {
					t.Errorf("pending id %q want %q", fr.ID, approvedID)
				}
			}
		}
		gotStdout, _, exitCode := collectStreams(t, res.frames)
		if !sawPending {
			t.Error("missing pending frame")
		}
		if strings.TrimSpace(gotStdout) != "approved" {
			t.Errorf("stdout %q", gotStdout)
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

// TestBroker_StreamsOutputIncrementally exercises the streaming path:
// a bash one-liner emits two lines with a sleep between them. The
// stdout frames must arrive before the exit frame, and at least one
// stdout frame must arrive before the second line is even written by
// the child (i.e., the first echo's frame shows up while the sleep
// is still in progress). Without streaming this test cannot pass —
// buffering would deliver everything after cmd.Wait returns.
func TestBroker_StreamsOutputIncrementally(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled: true,
		Rules: []Rule{
			{Match: []string{"bash", "-c", "echo first; sleep 0.2; echo second"}, Action: ActionAutoAllow},
		},
	}
	b := startTestBroker(t, cfg, projectDir)

	conn, err := net.Dial("unix", b.BrokerSocketPath())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	req := brokerRequest{
		V: 1, Argv: []string{"bash", "-c", "echo first; sleep 0.2; echo second"}, Cwd: projectDir,
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}

	dec := json.NewDecoder(conn)
	start := time.Now()

	// First stdout frame must arrive well before the child's 200ms sleep
	// elapses. If we still see nothing after 150ms, streaming is broken.
	var first brokerFrame
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("decode first frame: %v", err)
	}
	if first.Type != frameTypeStdout {
		t.Fatalf("first frame = %+v, want stdout", first)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("first stdout frame took %v; expected to arrive before the child's sleep ended (output was buffered)", elapsed)
	}
	if !strings.Contains(first.Data, "first") {
		t.Errorf("first stdout frame = %q, want it to contain %q", first.Data, "first")
	}

	// Drain the rest. We don't care how many frames the second line
	// arrives in, only that "second" is in there and exit comes last.
	var rest []brokerFrame
	for {
		var fr brokerFrame
		if err := dec.Decode(&fr); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode: %v", err)
		}
		rest = append(rest, fr)
	}
	all := append([]brokerFrame{first}, rest...)
	stdout, _, exit := collectStreams(t, all)
	if !strings.Contains(stdout, "second") {
		t.Errorf("stdout missing %q; got %q", "second", stdout)
	}
	if exit == nil || *exit != 0 {
		t.Errorf("exit = %v, want 0", exit)
	}
	if all[len(all)-1].Type != frameTypeExit {
		t.Errorf("last frame = %+v, want exit", all[len(all)-1])
	}
}

// TestBroker_MultiBroker_ApproverRoutesPerSocket pins the invariant
// that each broker's approve.sock owns only its own pending queue:
// two brokers running in parallel must not see each other's pending
// requests, and a decide on broker A must not resolve a request
// belonging to broker B. This is the contract `bwai approve` relies
// on when it fans out across /tmp/bwai-*/approve.sock — if it broke,
// the approver would route decisions to the wrong broker and confirms
// would silently time out.
func TestBroker_MultiBroker_ApproverRoutesPerSocket(t *testing.T) {
	projectDirA := t.TempDir()
	projectDirB := t.TempDir()
	cfg := BrokerConfig{
		Enabled:          true,
		ApprovalTimeoutS: 5,
		Rules:            []Rule{{Match: []string{"echo", "multi"}, Action: ActionConfirm}},
	}
	bA := startTestBroker(t, cfg, projectDirA)
	bB := startTestBroker(t, cfg, projectDirB)

	resA := make(chan []brokerFrame, 1)
	resB := make(chan []brokerFrame, 1)
	go func() {
		resA <- sendRequest(t, bA.BrokerSocketPath(), brokerRequest{
			V: 1, Argv: []string{"echo", "multi"}, Cwd: projectDirA,
		})
	}()
	go func() {
		resB <- sendRequest(t, bB.BrokerSocketPath(), brokerRequest{
			V: 1, Argv: []string{"echo", "multi"}, Cwd: projectDirB,
		})
	}()

	// Wait for both to enqueue, then snapshot each approve.sock and
	// verify the queues are disjoint and project-tagged correctly.
	listOne := func(sockPath string) []approverPending {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("dial %s: %v", sockPath, err)
		}
		defer conn.Close()
		if err := json.NewEncoder(conn).Encode(approverRequest{Op: "list"}); err != nil {
			t.Fatalf("encode list: %v", err)
		}
		var reply approverReply
		if err := json.NewDecoder(conn).Decode(&reply); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return reply.Pending
	}

	var pendA, pendB []approverPending
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pendA = listOne(bA.ApproveSocketPath())
		pendB = listOne(bB.ApproveSocketPath())
		if len(pendA) == 1 && len(pendB) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(pendA) != 1 || len(pendB) != 1 {
		t.Fatalf("expected 1 pending per broker, got A=%d B=%d", len(pendA), len(pendB))
	}
	if pendA[0].ID == pendB[0].ID {
		t.Fatalf("ids collided across brokers: %s", pendA[0].ID)
	}
	if pendA[0].ProjectDir != projectDirA {
		t.Errorf("brokerA project_dir = %q, want %q", pendA[0].ProjectDir, projectDirA)
	}
	if pendB[0].ProjectDir != projectDirB {
		t.Errorf("brokerB project_dir = %q, want %q", pendB[0].ProjectDir, projectDirB)
	}

	// Cross-broker decide must fail: brokerA does not own brokerB's id.
	{
		conn, err := net.Dial("unix", bA.ApproveSocketPath())
		if err != nil {
			t.Fatalf("dial A: %v", err)
		}
		_ = json.NewEncoder(conn).Encode(approverRequest{Op: "decide", ID: pendB[0].ID, Decision: "approve"})
		var reply approverReply
		_ = json.NewDecoder(conn).Decode(&reply)
		conn.Close()
		if reply.OK {
			t.Fatalf("brokerA accepted decide for brokerB's id %s", pendB[0].ID)
		}
	}

	// Approve each on its own socket; both client requests should
	// complete successfully.
	approveOn := func(sockPath, id, decision string) {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("dial %s: %v", sockPath, err)
		}
		defer conn.Close()
		_ = json.NewEncoder(conn).Encode(approverRequest{Op: "decide", ID: id, Decision: decision})
		var reply approverReply
		_ = json.NewDecoder(conn).Decode(&reply)
		if !reply.OK {
			t.Fatalf("decide on %s failed: %s", sockPath, reply.Error)
		}
	}
	approveOn(bA.ApproveSocketPath(), pendA[0].ID, "approve")
	approveOn(bB.ApproveSocketPath(), pendB[0].ID, "approve")

	for _, ch := range []chan []brokerFrame{resA, resB} {
		select {
		case frames := <-ch:
			_, _, exit := collectStreams(t, frames)
			if exit == nil || *exit != 0 {
				t.Errorf("exit = %v, want 0; frames = %+v", exit, frames)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("client never returned")
		}
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
