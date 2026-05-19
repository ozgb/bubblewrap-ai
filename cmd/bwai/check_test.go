package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintCheckResult(t *testing.T) {
	rules := []Rule{
		{Match: []string{"git", "push", "--force", "**"}, Action: ActionAutoDeny},
		{Match: []string{"git", "**"}, Action: ActionAutoAllow},
	}

	cases := []struct {
		name     string
		argv     []string
		wantCode int
		wantSubs []string
	}{
		{
			name:     "auto_deny rule match",
			argv:     []string{"git", "push", "--force", "main"},
			wantCode: 1,
			wantSubs: []string{"matched: rules[0]", "AUTO_DENY"},
		},
		{
			name:     "auto_allow rule match",
			argv:     []string{"git", "status"},
			wantCode: 0,
			wantSubs: []string{"matched: rules[1]", "AUTO_ALLOW"},
		},
		{
			name:     "implicit deny",
			argv:     []string{"hg", "status"},
			wantCode: 1,
			wantSubs: []string{"matched: (none)", "AUTO_DENY (implicit)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			code := printCheckResult(&buf, rules, tc.argv)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			out := buf.String()
			for _, sub := range tc.wantSubs {
				if !strings.Contains(out, sub) {
					t.Errorf("output missing %q\n--- got ---\n%s", sub, out)
				}
			}
		})
	}
}
