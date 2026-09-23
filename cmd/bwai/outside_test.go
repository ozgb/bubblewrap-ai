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
		// Rows carry their rules[N] index so denials and --check output
		// can be cross-referenced against this view.
		"rules[0]", "rules[1]", "rules[2]",
		// The footer teaches token-position matching by example: a flag
		// shifts every later token out from under a fixed-token pattern.
		"does NOT match `gh issue create **`",
		"--check",
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

// sendCheck is a wire-level dry run: it does not need a valid cwd,
// because matching is a pure function of the rule list and argv.
func sendCheck(t *testing.T, b *Broker, argv []string) brokerFrame {
	t.Helper()
	conn, err := net.Dial("unix", b.BrokerSocketPath())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(brokerRequest{V: 1, Op: opCheck, Argv: argv}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var fr brokerFrame
	if err := json.NewDecoder(conn).Decode(&fr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return fr
}

func TestBroker_CheckOp(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled: true,
		Rules: []Rule{
			{Match: []string{"gh", "issue", "create", "**"}, Action: ActionConfirm},
			{Match: []string{"gh", "issue", "-R", "CubeB/RWE-B4", "create", "**"}, Action: ActionAutoAllow},
			{Match: []string{"gh", "secret", "**"}, Action: ActionAutoDeny},
		},
	}
	b := startTestBroker(t, cfg, projectDir)

	cases := []struct {
		name     string
		argv     []string
		wantIdx  int
		wantAct  string
		wantRule bool
	}{
		{
			name:     "flag shifts every later token, so the narrow rule wins",
			argv:     []string{"gh", "issue", "-R", "CubeB/RWE-B4", "create", "title"},
			wantIdx:  1,
			wantAct:  ActionAutoAllow,
			wantRule: true,
		},
		{
			name:     "without the flag the broad confirm rule matches",
			argv:     []string{"gh", "issue", "create", "title"},
			wantIdx:  0,
			wantAct:  ActionConfirm,
			wantRule: true,
		},
		{
			name:     "explicit auto_deny rule carries its identity",
			argv:     []string{"gh", "secret", "list"},
			wantIdx:  2,
			wantAct:  ActionAutoDeny,
			wantRule: true,
		},
		{
			name:     "no match is the implicit deny",
			argv:     []string{"hg", "push"},
			wantIdx:  -1,
			wantAct:  ActionAutoDeny,
			wantRule: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := sendCheck(t, b, tc.argv)
			if fr.Type != frameTypeCheck {
				t.Fatalf("frame type = %q, want %q", fr.Type, frameTypeCheck)
			}
			if fr.Matched == nil {
				t.Fatal("check reply carried no matched rule")
			}
			m := fr.Matched
			if m.Idx != tc.wantIdx || m.Action != tc.wantAct {
				t.Errorf("matched = {idx %d, action %q}, want {idx %d, action %q}", m.Idx, m.Action, tc.wantIdx, tc.wantAct)
			}
			if tc.wantRule {
				if m.Rule == nil || strings.Join(m.Rule.Match, " ") != strings.Join(cfg.Rules[tc.wantIdx].Match, " ") {
					t.Errorf("matched rule = %+v, want %+v", m.Rule, cfg.Rules[tc.wantIdx])
				}
			} else if m.Rule != nil {
				t.Errorf("implicit deny should carry no rule, got %+v", m.Rule)
			}
		})
	}
}

func TestBroker_CheckMirrorsSessionPromotion(t *testing.T) {
	// A dry run must report the action a real exec would take, so a
	// confirm rule whose argv was already approved "always" reads as
	// auto_allow.
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled: true,
		Rules:   []Rule{{Match: []string{"echo", "twice"}, Action: ActionConfirm}},
	}
	b := startTestBroker(t, cfg, projectDir)

	if fr := sendCheck(t, b, []string{"echo", "twice"}); fr.Matched.Action != ActionConfirm {
		t.Fatalf("before promotion: action = %q, want %q", fr.Matched.Action, ActionConfirm)
	}
	b.recordSessionAllow([]string{"echo", "twice"})
	if fr := sendCheck(t, b, []string{"echo", "twice"}); fr.Matched.Action != ActionAutoAllow {
		t.Fatalf("after promotion: action = %q, want %q", fr.Matched.Action, ActionAutoAllow)
	}
}

func TestBroker_CheckOpRejectsEmptyArgv(t *testing.T) {
	projectDir := t.TempDir()
	b := startTestBroker(t, BrokerConfig{Enabled: true}, projectDir)
	conn, err := net.Dial("unix", b.BrokerSocketPath())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(brokerRequest{V: 1, Op: opCheck}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var fr brokerFrame
	if err := json.NewDecoder(conn).Decode(&fr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fr.Type != frameTypeDenied || fr.Reason != denyReasonInvalid {
		t.Fatalf("expected deny:invalid for empty argv, got %+v", fr)
	}
}

func TestBroker_DeniedFrameCarriesMatchedRule(t *testing.T) {
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled: true,
		Rules:   []Rule{{Match: []string{"gh", "secret", "**"}, Action: ActionAutoDeny}},
	}
	b := startTestBroker(t, cfg, projectDir)
	frames := sendRequest(t, b.BrokerSocketPath(), brokerRequest{
		V: 1, Argv: []string{"gh", "secret", "list"}, Cwd: projectDir,
	})
	if len(frames) != 1 || frames[0].Type != frameTypeDenied || frames[0].Reason != denyReasonRule {
		t.Fatalf("expected single deny:rule frame, got %+v", frames)
	}
	m := frames[0].Matched
	if m == nil || m.Idx != 0 || m.Rule == nil || m.Action != ActionAutoDeny {
		t.Fatalf("denied frame matched = %+v, want rules[0] auto_deny", m)
	}
	if strings.Join(m.Rule.Match, " ") != "gh secret **" {
		t.Errorf("matched rule pattern = %+v, want gh secret **", m.Rule.Match)
	}
}
