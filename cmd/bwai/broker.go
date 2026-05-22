package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Wire protocol — sandbox → host (broker.sock).
//
// Op selects the request kind. Backward-compat: an absent or empty
// Op means "exec" (the original behaviour).
type brokerRequest struct {
	V            int      `json:"v"`
	Op           string   `json:"op,omitempty"` // "exec" (default) | "list_rules"
	Argv         []string `json:"argv"`
	Cwd          string   `json:"cwd"`
	StdinInherit bool     `json:"stdin_inherit"`
}

const (
	opExec      = "exec"
	opListRules = "list_rules"
)

// Wire protocol — host → sandbox reply frames. Pointer for Code so the
// difference between "exit 0" and "no exit yet" survives JSON encoding.
type brokerFrame struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitempty"`
	Data   string `json:"data,omitempty"`
	Code   *int   `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	Rules  []Rule `json:"rules,omitempty"` // populated only for frameTypeRules
}

const (
	frameTypePending = "pending"
	frameTypeStdout  = "stdout"
	frameTypeStderr  = "stderr"
	frameTypeExit    = "exit"
	frameTypeDenied  = "denied"
	frameTypeRules   = "rules"
)

const (
	denyReasonRule      = "rule"
	denyReasonUser      = "user"
	denyReasonTimeout   = "timeout"
	denyReasonRateLimit = "ratelimit"
	denyReasonInvalid   = "invalid"
)

// Defaults that match the doc.
const (
	defaultApprovalTimeoutSec = 120
	rateLimitMinIntervalMs    = 2000
	rateLimitConfirmsPerSess  = 30
)

// pendingRequest sits in the broker's queue waiting for an approver
// decision. The decision chan is closed by the approver path.
type pendingRequest struct {
	id       string
	req      brokerRequest
	matchIdx int
	enqueued time.Time
	decision chan string // "approve" | "deny"
	once     sync.Once
}

// resolve delivers a decision to the waiting request exactly once.
func (p *pendingRequest) resolve(decision string) {
	p.once.Do(func() {
		p.decision <- decision
		close(p.decision)
	})
}

// Broker owns the host-side state for one bwai run. It listens on two
// sockets (broker.sock for the sandbox client, approve.sock for the
// approver client) and serializes confirm prompts through a pending
// queue.
type Broker struct {
	cfg         BrokerConfig
	projectDir  string
	auditLog    *auditLogger
	brokerLn    net.Listener
	approveLn   net.Listener
	tmpDir      string
	mu          sync.Mutex
	pending     map[string]*pendingRequest
	sessAllow   [][]string // per-session "always-this-session" allowlist
	confirmHist []time.Time
}

// NewBroker prepares the tmpdir and listeners. The caller is
// responsible for invoking Serve in a goroutine and Close on shutdown.
func NewBroker(cfg BrokerConfig, projectDir string, auditPath string) (*Broker, error) {
	tmpDir := fmt.Sprintf("/tmp/bwai-%d", os.Getpid())
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("create tmpdir: %w", err)
	}
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod tmpdir: %w", err)
	}
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return nil, fmt.Errorf("create bin dir: %w", err)
	}

	brokerSock := filepath.Join(tmpDir, "broker.sock")
	approveSock := filepath.Join(tmpDir, "approve.sock")
	_ = os.Remove(brokerSock)
	_ = os.Remove(approveSock)

	brokerLn, err := net.Listen("unix", brokerSock)
	if err != nil {
		return nil, fmt.Errorf("listen broker.sock: %w", err)
	}
	if err := os.Chmod(brokerSock, 0o600); err != nil {
		_ = brokerLn.Close()
		return nil, fmt.Errorf("chmod broker.sock: %w", err)
	}
	approveLn, err := net.Listen("unix", approveSock)
	if err != nil {
		_ = brokerLn.Close()
		return nil, fmt.Errorf("listen approve.sock: %w", err)
	}
	if err := os.Chmod(approveSock, 0o600); err != nil {
		_ = brokerLn.Close()
		_ = approveLn.Close()
		return nil, fmt.Errorf("chmod approve.sock: %w", err)
	}

	audit, err := newAuditLogger(auditPath)
	if err != nil {
		_ = brokerLn.Close()
		_ = approveLn.Close()
		return nil, fmt.Errorf("open audit log: %w", err)
	}

	if cfg.ApprovalTimeoutS <= 0 {
		cfg.ApprovalTimeoutS = defaultApprovalTimeoutSec
	}

	return &Broker{
		cfg:        cfg,
		projectDir: projectDir,
		auditLog:   audit,
		brokerLn:   brokerLn,
		approveLn:  approveLn,
		tmpDir:     tmpDir,
		pending:    map[string]*pendingRequest{},
	}, nil
}

// TmpDir is the host-side path bind-mounted into the sandbox at
// /run/bwai/.
func (b *Broker) TmpDir() string { return b.tmpDir }

// BrokerSocketPath is the host-side path of broker.sock. The sandbox
// sees this at /run/bwai/broker.sock.
func (b *Broker) BrokerSocketPath() string {
	return filepath.Join(b.tmpDir, "broker.sock")
}

// ApproveSocketPath is the host-side path of approve.sock. Never
// bind-mounted into the sandbox.
func (b *Broker) ApproveSocketPath() string {
	return filepath.Join(b.tmpDir, "approve.sock")
}

// Serve runs both accept loops until Close is called. Returns when both
// listeners are closed.
func (b *Broker) Serve() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b.serveBroker()
	}()
	go func() {
		defer wg.Done()
		b.serveApprover()
	}()
	wg.Wait()
}

// Close shuts both listeners down and removes the tmpdir. Safe to call
// more than once.
func (b *Broker) Close() error {
	var firstErr error
	if b.brokerLn != nil {
		if err := b.brokerLn.Close(); err != nil {
			firstErr = err
		}
	}
	if b.approveLn != nil {
		if err := b.approveLn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if b.auditLog != nil {
		if err := b.auditLog.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if b.tmpDir != "" {
		if err := os.RemoveAll(b.tmpDir); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *Broker) serveBroker() {
	for {
		conn, err := b.brokerLn.Accept()
		if err != nil {
			if isClosedConn(err) {
				return
			}
			continue
		}
		go b.handleBrokerConn(conn)
	}
}

func (b *Broker) handleBrokerConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req brokerRequest
	if err := dec.Decode(&req); err != nil {
		_ = enc.Encode(brokerFrame{Type: frameTypeDenied, Reason: denyReasonInvalid})
		return
	}
	if req.V != 1 {
		_ = enc.Encode(brokerFrame{Type: frameTypeDenied, Reason: denyReasonInvalid})
		return
	}
	// Op dispatch. Empty/missing Op == "exec" for backward compat.
	switch req.Op {
	case "", opExec:
		// fall through to the exec path below
	case opListRules:
		_ = enc.Encode(brokerFrame{Type: frameTypeRules, Rules: b.cfg.Rules})
		return
	default:
		_ = enc.Encode(brokerFrame{Type: frameTypeDenied, Reason: denyReasonInvalid})
		return
	}
	if len(req.Argv) == 0 {
		_ = enc.Encode(brokerFrame{Type: frameTypeDenied, Reason: denyReasonInvalid})
		return
	}
	if !b.cwdAllowed(req.Cwd) {
		b.auditLog.write(auditEntry{Argv: req.Argv, Cwd: req.Cwd, Decision: "denied:" + denyReasonInvalid})
		_ = enc.Encode(brokerFrame{Type: frameTypeDenied, Reason: denyReasonInvalid})
		return
	}

	action, idx := matchRules(b.cfg.Rules, req.Argv)
	// "always-this-session" promotes a previously-confirmed argv to auto_allow
	// for the lifetime of this broker.
	if action == ActionConfirm && b.isSessionAllowed(req.Argv) {
		action = ActionAutoAllow
	}

	switch action {
	case ActionAutoDeny:
		b.auditLog.write(auditEntry{Argv: req.Argv, Cwd: req.Cwd, MatchedRule: idx, Decision: "denied:" + denyReasonRule})
		_ = enc.Encode(brokerFrame{Type: frameTypeDenied, Reason: denyReasonRule})
		return
	case ActionAutoAllow:
		b.execAndStream(enc, req, idx, "auto_allow")
		return
	case ActionConfirm:
		if !b.checkRateLimit() {
			b.auditLog.write(auditEntry{Argv: req.Argv, Cwd: req.Cwd, MatchedRule: idx, Decision: "denied:" + denyReasonRateLimit})
			_ = enc.Encode(brokerFrame{Type: frameTypeDenied, Reason: denyReasonRateLimit})
			return
		}
		decision := b.awaitApproval(req, idx, enc)
		switch decision {
		case "approve":
			b.execAndStream(enc, req, idx, "confirm")
		case "always":
			b.recordSessionAllow(req.Argv)
			b.execAndStream(enc, req, idx, "confirm:always")
		case "deny":
			b.auditLog.write(auditEntry{Argv: req.Argv, Cwd: req.Cwd, MatchedRule: idx, Decision: "denied:" + denyReasonUser})
			_ = enc.Encode(brokerFrame{Type: frameTypeDenied, Reason: denyReasonUser})
		case "timeout":
			b.auditLog.write(auditEntry{Argv: req.Argv, Cwd: req.Cwd, MatchedRule: idx, Decision: "denied:" + denyReasonTimeout})
			_ = enc.Encode(brokerFrame{Type: frameTypeDenied, Reason: denyReasonTimeout})
		}
	}
}

// awaitApproval enqueues req, sends the pending frame, and blocks until
// the approver responds or the timeout fires.
func (b *Broker) awaitApproval(req brokerRequest, matchIdx int, enc *json.Encoder) string {
	p := &pendingRequest{
		id:       newRequestID(),
		req:      req,
		matchIdx: matchIdx,
		enqueued: time.Now(),
		decision: make(chan string, 1),
	}
	b.mu.Lock()
	b.pending[p.id] = p
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, p.id)
		b.mu.Unlock()
	}()

	if err := enc.Encode(brokerFrame{Type: frameTypePending, ID: p.id}); err != nil {
		return "deny"
	}

	select {
	case d := <-p.decision:
		return d
	case <-time.After(time.Duration(b.cfg.ApprovalTimeoutS) * time.Second):
		// once.Do means whoever fired resolve() first wins — if an
		// approver decision raced the timer, that decision is already
		// in the buffered channel. Read it back so we honour it
		// rather than always reporting "timeout".
		p.resolve("timeout")
		return <-p.decision
	}
}

// execAndStream runs argv on the host (host env, request cwd) and
// streams stdout/stderr/exit frames back as data arrives.
//
// Each pipe read becomes one frame, so a long-running command shows
// incremental output to the sandbox client instead of being buffered
// until exit. Frames can interleave at chunk boundaries between
// stdout and stderr — that matches real terminal behaviour.
func (b *Broker) execAndStream(enc *json.Encoder, req brokerRequest, matchIdx int, decision string) {
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Cwd
	cmd.Env = os.Environ() // host env, not sandbox env

	// Acquire pipes before Start; Wait closes them so readers must
	// drain to EOF before Wait returns.
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		b.emitStartFailure(enc, req, matchIdx, decision, err)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		b.emitStartFailure(enc, req, matchIdx, decision, err)
		return
	}
	if err := cmd.Start(); err != nil {
		b.emitStartFailure(enc, req, matchIdx, decision, err)
		return
	}

	// json.Encoder is not safe for concurrent use; serialize writes
	// from both reader goroutines through a mutex.
	var encMu sync.Mutex
	emit := func(fr brokerFrame) {
		encMu.Lock()
		defer encMu.Unlock()
		_ = enc.Encode(fr)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		streamPipe(stdoutPipe, frameTypeStdout, emit)
	}()
	go func() {
		defer wg.Done()
		streamPipe(stderrPipe, frameTypeStderr, emit)
	}()
	wg.Wait()

	exitCode := 0
	if runErr := cmd.Wait(); runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
			emit(brokerFrame{Type: frameTypeStderr, Data: "bwai broker: " + runErr.Error() + "\n"})
		}
	}
	code := exitCode
	emit(brokerFrame{Type: frameTypeExit, Code: &code})

	b.auditLog.write(auditEntry{
		Argv:        req.Argv,
		Cwd:         req.Cwd,
		MatchedRule: matchIdx,
		Decision:    decision,
		ExitCode:    &code,
	})
}

// streamPipe reads chunks from r and turns each non-empty read into a
// frame of the given type. Stops on EOF or read error.
func streamPipe(r io.Reader, frameType string, emit func(brokerFrame)) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			emit(brokerFrame{Type: frameType, Data: string(buf[:n])})
		}
		if err != nil {
			return
		}
	}
}

// emitStartFailure synthesizes the stderr+exit frame pair when the
// child never started (bad path, fork failure, pipe alloc failure).
// We can't ask the OS for an exit code in that case, so we use -1 to
// match the previous behaviour.
func (b *Broker) emitStartFailure(enc *json.Encoder, req brokerRequest, matchIdx int, decision string, runErr error) {
	_ = enc.Encode(brokerFrame{Type: frameTypeStderr, Data: "bwai broker: " + runErr.Error() + "\n"})
	code := -1
	_ = enc.Encode(brokerFrame{Type: frameTypeExit, Code: &code})
	b.auditLog.write(auditEntry{
		Argv:        req.Argv,
		Cwd:         req.Cwd,
		MatchedRule: matchIdx,
		Decision:    decision,
		ExitCode:    &code,
	})
}

// cwdAllowed enforces that the request runs inside the project bind
// mount. Symlinks inside the sandbox could let a malicious agent steer
// us elsewhere; check against the cleaned, absolute path.
func (b *Broker) cwdAllowed(cwd string) bool {
	if !filepath.IsAbs(cwd) {
		return false
	}
	clean := filepath.Clean(cwd)
	rel, err := filepath.Rel(b.projectDir, clean)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..")
}

// checkRateLimit enforces both the inter-confirm minimum interval and
// the per-session cap. Confirms only — auto_allow is unbounded.
func (b *Broker) checkRateLimit() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if len(b.confirmHist) >= rateLimitConfirmsPerSess {
		return false
	}
	if n := len(b.confirmHist); n > 0 {
		if now.Sub(b.confirmHist[n-1]) < time.Duration(rateLimitMinIntervalMs)*time.Millisecond {
			return false
		}
	}
	b.confirmHist = append(b.confirmHist, now)
	return true
}

func (b *Broker) recordSessionAllow(argv []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]string, len(argv))
	copy(cp, argv)
	b.sessAllow = append(b.sessAllow, cp)
}

func (b *Broker) isSessionAllowed(argv []string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, a := range b.sessAllow {
		if argvEqual(a, argv) {
			return true
		}
	}
	return false
}

func argvEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Approver protocol — host-only, used by `bwai approve`. NDJSON over
// approve.sock. Op kinds: "list" returns the pending queue; "decide"
// resolves one request.
type approverRequest struct {
	Op       string `json:"op"`
	ID       string `json:"id,omitempty"`
	Decision string `json:"decision,omitempty"` // "approve" | "deny" | "always"
}

type approverPending struct {
	ID         string   `json:"id"`
	Argv       []string `json:"argv"`
	Cwd        string   `json:"cwd"`
	AgeMs      int64    `json:"age_ms"`
	ProjectDir string   `json:"project_dir,omitempty"`
}

type approverReply struct {
	OK      bool              `json:"ok"`
	Error   string            `json:"error,omitempty"`
	Pending []approverPending `json:"pending,omitempty"`
}

func (b *Broker) serveApprover() {
	for {
		conn, err := b.approveLn.Accept()
		if err != nil {
			if isClosedConn(err) {
				return
			}
			continue
		}
		go b.handleApproverConn(conn)
	}
}

func (b *Broker) handleApproverConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	dec := json.NewDecoder(r)
	enc := json.NewEncoder(conn)

	for {
		var req approverRequest
		if err := dec.Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) {
				_ = enc.Encode(approverReply{Error: "decode: " + err.Error()})
			}
			return
		}
		switch req.Op {
		case "list":
			_ = enc.Encode(approverReply{OK: true, Pending: b.snapshotPending()})
		case "decide":
			if req.Decision != "approve" && req.Decision != "deny" && req.Decision != "always" {
				_ = enc.Encode(approverReply{Error: "invalid decision"})
				continue
			}
			b.mu.Lock()
			p, ok := b.pending[req.ID]
			b.mu.Unlock()
			if !ok {
				_ = enc.Encode(approverReply{Error: "no such id"})
				continue
			}
			p.resolve(req.Decision)
			_ = enc.Encode(approverReply{OK: true})
		default:
			_ = enc.Encode(approverReply{Error: "unknown op"})
		}
	}
}

func (b *Broker) snapshotPending() []approverPending {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]approverPending, 0, len(b.pending))
	now := time.Now()
	for _, p := range b.pending {
		out = append(out, approverPending{
			ID:         p.id,
			Argv:       p.req.Argv,
			Cwd:        p.req.Cwd,
			AgeMs:      now.Sub(p.enqueued).Milliseconds(),
			ProjectDir: b.projectDir,
		})
	}
	return out
}

func newRequestID() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func isClosedConn(err error) bool {
	return errors.Is(err, net.ErrClosed)
}
