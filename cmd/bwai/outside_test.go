package main

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"
)

func TestPrintRules(t *testing.T) {
	rules := []Rule{
		{Match: []string{"git", "push", "--force", "**"}, Action: ActionAutoDeny},
		{Match: []string{"git", "status"}, Action: ActionAutoAllow},
		{Match: []string{"git", "commit", "**"}, Action: ActionConfirm},
	}
	var buf bytes.Buffer
	printRules(&buf, rules)
	out := buf.String()
	for _, want := range []string{
		"AUTO_DENY", "git push --force **",
		"AUTO_ALLOW", "git status",
		"CONFIRM", "git commit **",
		"First-match wins",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("printRules output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestPrintRulesEmpty(t *testing.T) {
	var buf bytes.Buffer
	printRules(&buf, nil)
	if !strings.Contains(buf.String(), "no rules configured") {
		t.Errorf("empty rule set should advertise the deny default: %q", buf.String())
	}
}

func TestBroker_ListRulesOp(t *testing.T) {
	projectDir := t.TempDir()
	want := []Rule{
		{Match: []string{"git", "status"}, Action: ActionAutoAllow},
		{Match: []string{"git", "commit", "**"}, Action: ActionConfirm},
	}
	cfg := BrokerConfig{Enabled: true, Rules: want}
	b := startTestBroker(t, cfg, projectDir)

	conn, err := net.Dial("unix", b.BrokerSocketPath())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(brokerRequest{V: 1, Op: opListRules}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var fr brokerFrame
	if err := json.NewDecoder(conn).Decode(&fr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fr.Type != frameTypeRules {
		t.Fatalf("frame type = %q, want %q", fr.Type, frameTypeRules)
	}
	if len(fr.Rules) != len(want) {
		t.Fatalf("got %d rules, want %d", len(fr.Rules), len(want))
	}
	for i, r := range fr.Rules {
		if r.Action != want[i].Action || strings.Join(r.Match, " ") != strings.Join(want[i].Match, " ") {
			t.Errorf("rule[%d] = %+v, want %+v", i, r, want[i])
		}
	}
}

func TestBroker_ListRulesPreservesExecBackwardCompat(t *testing.T) {
	// A request with no Op field (the v1 wire format pre-list_rules)
	// must still route through the exec path. Belt-and-braces test:
	// the JSON literal here matches what an old client sends.
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled: true,
		Rules:   []Rule{{Match: []string{"echo", "back-compat"}, Action: ActionAutoAllow}},
	}
	b := startTestBroker(t, cfg, projectDir)

	conn, err := net.Dial("unix", b.BrokerSocketPath())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	payload := map[string]any{
		"v":    1,
		"argv": []string{"echo", "back-compat"},
		"cwd":  projectDir,
	}
	if err := json.NewEncoder(conn).Encode(payload); err != nil {
		t.Fatalf("send: %v", err)
	}
	dec := json.NewDecoder(conn)
	var sawStdout bool
	for {
		var fr brokerFrame
		if err := dec.Decode(&fr); err != nil {
			break
		}
		if fr.Type == frameTypeStdout && strings.Contains(fr.Data, "back-compat") {
			sawStdout = true
		}
		if fr.Type == frameTypeDenied {
			t.Fatalf("v1-style request was denied: %+v", fr)
		}
	}
	if !sawStdout {
		t.Error("v1-style exec request did not stream stdout")
	}
}
