package main

const (
	ActionAutoAllow = "auto_allow"
	ActionConfirm   = "confirm"
	ActionAutoDeny  = "auto_deny"
)

// Rule is one entry in broker.rules. MVP: literal-argv match only.
// Globs (`*` / `**`) are reserved for a follow-up PR — they are
// rejected at config-load time so a typo doesn't silently fall through
// to implicit deny.
type Rule struct {
	Match  []string `json:"match"`
	Action string   `json:"action"`
}

// matchRules walks rules in order, returning the first action that
// applies and its index. No match → implicit auto_deny (idx -1).
func matchRules(rules []Rule, argv []string) (action string, idx int) {
	for i, r := range rules {
		if literalMatch(r.Match, argv) {
			return r.Action, i
		}
	}
	return ActionAutoDeny, -1
}

func literalMatch(pattern, argv []string) bool {
	if len(pattern) != len(argv) {
		return false
	}
	for i, tok := range pattern {
		if tok != argv[i] {
			return false
		}
	}
	return true
}

// validateRule is called at config load. Rejects unknown actions and
// any pattern containing a wildcard token (the follow-up PR will add
// glob support; surfacing the limitation as an error is friendlier
// than silently never-matching).
func validateRule(r Rule) error {
	switch r.Action {
	case ActionAutoAllow, ActionConfirm, ActionAutoDeny:
	default:
		return &ruleErr{msg: "unknown action: " + r.Action}
	}
	if len(r.Match) == 0 {
		return &ruleErr{msg: "match must be non-empty"}
	}
	for _, tok := range r.Match {
		if tok == "*" || tok == "**" {
			return &ruleErr{msg: "wildcard patterns (*, **) not yet supported; literal-argv rules only in this release"}
		}
	}
	return nil
}

type ruleErr struct{ msg string }

func (e *ruleErr) Error() string { return e.msg }
