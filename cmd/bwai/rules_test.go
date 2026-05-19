package main

import "testing"

func TestMatchRulesLiteral(t *testing.T) {
	rules := []Rule{
		{Match: []string{"git", "push", "--force"}, Action: ActionAutoDeny},
		{Match: []string{"git", "status"}, Action: ActionAutoAllow},
		{Match: []string{"git", "commit", "-m", "fix"}, Action: ActionConfirm},
	}

	cases := []struct {
		name    string
		argv    []string
		wantAct string
		wantIdx int
	}{
		{"exact status", []string{"git", "status"}, ActionAutoAllow, 1},
		{"force push hits deny", []string{"git", "push", "--force"}, ActionAutoDeny, 0},
		{"unmatched falls through to implicit deny", []string{"git", "log"}, ActionAutoDeny, -1},
		{"argc mismatch does not match", []string{"git", "status", "--short"}, ActionAutoDeny, -1},
		{"confirm rule", []string{"git", "commit", "-m", "fix"}, ActionConfirm, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAct, gotIdx := matchRules(rules, tc.argv)
			if gotAct != tc.wantAct || gotIdx != tc.wantIdx {
				t.Fatalf("matchRules(%v) = (%q, %d), want (%q, %d)", tc.argv, gotAct, gotIdx, tc.wantAct, tc.wantIdx)
			}
		})
	}
}

func TestMatchRulesFirstWins(t *testing.T) {
	rules := []Rule{
		{Match: []string{"git", "push"}, Action: ActionConfirm},
		{Match: []string{"git", "push"}, Action: ActionAutoDeny},
	}
	gotAct, gotIdx := matchRules(rules, []string{"git", "push"})
	if gotAct != ActionConfirm || gotIdx != 0 {
		t.Fatalf("first match should win: got (%q, %d)", gotAct, gotIdx)
	}
}

func TestMatchRulesGlobs(t *testing.T) {
	rules := []Rule{
		{Match: []string{"git", "push", "--force", "**"}, Action: ActionAutoDeny},
		{Match: []string{"git", "commit", "**"}, Action: ActionConfirm},
		{Match: []string{"git", "*", "--help"}, Action: ActionAutoAllow},
		{Match: []string{"git", "**"}, Action: ActionAutoAllow},
	}

	cases := []struct {
		name    string
		argv    []string
		wantAct string
		wantIdx int
	}{
		{"** consumes zero trailing", []string{"git", "commit"}, ActionConfirm, 1},
		{"** consumes many trailing", []string{"git", "commit", "-m", "msg"}, ActionConfirm, 1},
		{"force push hits deny before commit", []string{"git", "push", "--force", "main"}, ActionAutoDeny, 0},
		{"single * matches one slot", []string{"git", "status", "--help"}, ActionAutoAllow, 2},
		{"single * does not match zero slots", []string{"git", "--help"}, ActionAutoAllow, 3}, // falls through to git **
		{"trailing ** is a catch-all for the command", []string{"git", "log", "--oneline"}, ActionAutoAllow, 3},
		{"bare argv[0] no match without **", []string{"hg", "status"}, ActionAutoDeny, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAct, gotIdx := matchRules(rules, tc.argv)
			if gotAct != tc.wantAct || gotIdx != tc.wantIdx {
				t.Fatalf("matchRules(%v) = (%q, %d), want (%q, %d)", tc.argv, gotAct, gotIdx, tc.wantAct, tc.wantIdx)
			}
		})
	}
}

func TestValidateRule(t *testing.T) {
	cases := []struct {
		name    string
		rule    Rule
		wantErr bool
	}{
		{"valid auto_allow", Rule{Match: []string{"git", "status"}, Action: ActionAutoAllow}, false},
		{"valid single-star in middle", Rule{Match: []string{"git", "*", "--help"}, Action: ActionAutoAllow}, false},
		{"valid trailing double-star", Rule{Match: []string{"git", "commit", "**"}, Action: ActionConfirm}, false},
		{"unknown action", Rule{Match: []string{"git", "status"}, Action: "maybe"}, true},
		{"empty match", Rule{Match: nil, Action: ActionAutoAllow}, true},
		{"single-star in argv[0] rejected", Rule{Match: []string{"*", "status"}, Action: ActionAutoAllow}, true},
		{"double-star in argv[0] rejected", Rule{Match: []string{"**"}, Action: ActionAutoAllow}, true},
		{"double-star in middle rejected", Rule{Match: []string{"git", "**", "--help"}, Action: ActionAutoAllow}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRule(tc.rule)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
