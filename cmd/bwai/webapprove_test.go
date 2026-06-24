package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewToken(t *testing.T) {
	const want = 32 // 16 bytes hex-encoded
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		tok := newToken()
		if len(tok) != want {
			t.Fatalf("newToken() = %q (len %d), want len %d", tok, len(tok), want)
		}
		if seen[tok] {
			t.Fatalf("newToken() produced a duplicate: %q", tok)
		}
		seen[tok] = true
	}
}

// addPending injects a pending request directly into the broker queue so
// the HTTP handler can be exercised without a live confirm flow.
func addPending(b *Broker, id, token string, argv []string, cwd string) *pendingRequest {
	p := &pendingRequest{
		id:       id,
		token:    token,
		req:      brokerRequest{Argv: argv, Cwd: cwd},
		enqueued: time.Now(),
		decision: make(chan string, 1),
	}
	b.mu.Lock()
	b.pending[id] = p
	b.mu.Unlock()
	return p
}

func TestApprovalHandler_GETRendersPage(t *testing.T) {
	projectDir := t.TempDir()
	b := newTestBroker(t, BrokerConfig{Enabled: true}, projectDir)
	addPending(b, "abcd1234", "secrettoken", []string{"git", "push", "origin"}, projectDir)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/r/abcd1234?k=secrettoken", nil)
	b.approvalHandler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "git push origin") {
		t.Errorf("page body missing command; got %q", body)
	}
	// The token must be present so the POST form can carry it back.
	if !strings.Contains(body, "secrettoken") {
		t.Errorf("page body missing token field")
	}
}

func TestApprovalHandler_GETUnknownID404(t *testing.T) {
	b := newTestBroker(t, BrokerConfig{Enabled: true}, t.TempDir())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/r/nope?k=whatever", nil)
	b.approvalHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestApprovalHandler_GETWrongTokenForbidden(t *testing.T) {
	projectDir := t.TempDir()
	b := newTestBroker(t, BrokerConfig{Enabled: true}, projectDir)
	addPending(b, "abcd1234", "secrettoken", []string{"git", "push"}, projectDir)

	for _, tok := range []string{"", "wrongtoken", "secrettoke"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/r/abcd1234?k="+tok, nil)
		b.approvalHandler().ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("token %q: status = %d, want 403", tok, rr.Code)
		}
	}
}

func TestApprovalHandler_POSTApproveResolves(t *testing.T) {
	projectDir := t.TempDir()
	b := newTestBroker(t, BrokerConfig{Enabled: true}, projectDir)
	p := addPending(b, "abcd1234", "secrettoken", []string{"git", "push"}, projectDir)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/r/abcd1234",
		strings.NewReader("k=secrettoken&decision=approve"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	b.approvalHandler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	select {
	case d := <-p.decision:
		if d != "approve" {
			t.Errorf("decision = %q, want approve", d)
		}
	case <-time.After(time.Second):
		t.Fatal("POST did not resolve the pending request")
	}
}

func TestApprovalHandler_POSTWrongTokenForbidden(t *testing.T) {
	projectDir := t.TempDir()
	b := newTestBroker(t, BrokerConfig{Enabled: true}, projectDir)
	p := addPending(b, "abcd1234", "secrettoken", []string{"git", "push"}, projectDir)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/r/abcd1234",
		strings.NewReader("k=wrong&decision=approve"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	b.approvalHandler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	select {
	case d := <-p.decision:
		t.Fatalf("request resolved on a forbidden POST: %q", d)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestApprovalHandler_POSTInvalidDecision400(t *testing.T) {
	projectDir := t.TempDir()
	b := newTestBroker(t, BrokerConfig{Enabled: true}, projectDir)
	addPending(b, "abcd1234", "secrettoken", []string{"git", "push"}, projectDir)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/r/abcd1234",
		strings.NewReader("k=secrettoken&decision=maybe"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	b.approvalHandler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// TestApprovalHandler_ReplayAfterResolve404 simulates the single-use
// property: awaitApproval deletes the pending entry once it returns, so
// a second POST for the same id is a 404.
func TestApprovalHandler_ReplayAfterResolve404(t *testing.T) {
	projectDir := t.TempDir()
	b := newTestBroker(t, BrokerConfig{Enabled: true}, projectDir)
	addPending(b, "abcd1234", "secrettoken", []string{"git", "push"}, projectDir)

	post := func() int {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/r/abcd1234",
			strings.NewReader("k=secrettoken&decision=approve"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		b.approvalHandler().ServeHTTP(rr, req)
		return rr.Code
	}

	if code := post(); code != http.StatusOK {
		t.Fatalf("first POST status = %d, want 200", code)
	}
	// Simulate awaitApproval's cleanup deleting the resolved entry.
	b.mu.Lock()
	delete(b.pending, "abcd1234")
	b.mu.Unlock()
	if code := post(); code != http.StatusNotFound {
		t.Fatalf("replay POST status = %d, want 404", code)
	}
}

// firstPending polls the broker queue until a request appears and
// returns its id and token. Used by integration tests that drive a real
// confirm flow.
func firstPending(t *testing.T, b *Broker) (id, token string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		for _, p := range b.pending {
			id, token = p.id, p.token
		}
		b.mu.Unlock()
		if id != "" {
			return id, token
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pending request appeared")
	return "", ""
}

// TestWebApproval_EndToEnd drives a real confirm request through the
// broker and approves it via the HTTP handler, then confirms the replay
// is a 404 (single-use). Exercises the live pending→resolve→cleanup path
// rather than a hand-inserted entry.
func TestWebApproval_EndToEnd(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled:          true,
		Prompt:           []string{"web"},
		ApprovalTimeoutS: 5,
		Rules:            []Rule{{Match: []string{"echo", "web"}, Action: ActionConfirm}},
	}
	b := startTestBroker(t, cfg, projectDir)

	resCh := make(chan []brokerFrame, 1)
	go func() {
		resCh <- sendRequest(t, b.BrokerSocketPath(), brokerRequest{
			V: 1, Argv: []string{"echo", "web"}, Cwd: projectDir,
		})
	}()

	id, token := firstPending(t, b)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/r/"+id,
		strings.NewReader("k="+token+"&decision=approve"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	b.approvalHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve POST status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}

	select {
	case frames := <-resCh:
		stdout, _, exit := collectStreams(t, frames)
		if strings.TrimSpace(stdout) != "web" {
			t.Errorf("stdout = %q, want web", stdout)
		}
		if exit == nil || *exit != 0 {
			t.Errorf("exit = %v, want 0", exit)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client never returned after approval")
	}

	// The entry is gone once awaitApproval returned: a replay is a 404.
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/r/"+id,
		strings.NewReader("k="+token+"&decision=approve"))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	b.approvalHandler().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("replay POST status = %d, want 404", rr2.Code)
	}
}
