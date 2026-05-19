package main

const (
	ActionAutoAllow = "auto_allow"
	ActionConfirm   = "confirm"
	ActionAutoDeny  = "auto_deny"
)

const (
	tokSingle = "*"  // exactly one arg, any value
	tokRest   = "**" // zero or more args, only valid as the final token
)

// Rule is one entry in broker.rules. Patterns match the resolved argv
// array literally token-by-token, except for two wildcards:
//
//   - "*"  matches exactly one argv token
//   - "**" matches zero or more trailing tokens (last position only)
//
// argv[0] is always literal — wildcards in the command-name slot are
// rejected at config load. See docs/broker.md "Pattern matching".
type Rule struct {
	Match  []string `json:"match"`
	Action string   `json:"action"`
}

// matchRules walks rules in order, returning the first action that
// applies and its index. No match → implicit auto_deny (idx -1).
func matchRules(rules []Rule, argv []string) (action string, idx int) {
	for i, r := range rules {
		if patternMatch(r.Match, argv) {
			return r.Action, i
		}
	}
	return ActionAutoDeny, -1
}

// patternMatch implements the rule matcher described in docs/broker.md.
// Walks pattern and argv together. "**" consumes the remaining argv
// (and is only ever the last token; validateRule enforces that).
func patternMatch(pattern, argv []string) bool {
	for i, tok := range pattern {
		if tok == tokRest {
			// "**" is last (validated). Matches anything remaining,
			// including nothing.
			return true
		}
		if i >= len(argv) {
			return false
		}
		if tok == tokSingle {
			continue
		}
		if tok != argv[i] {
			return false
		}
	}
	return len(pattern) == len(argv)
}

// validateRule is called at config load. Rejects unknown actions,
// empty patterns, wildcards in argv[0], and "**" anywhere except the
// final position.
func validateRule(r Rule) error {
	switch r.Action {
	case ActionAutoAllow, ActionConfirm, ActionAutoDeny:
	default:
		return &ruleErr{msg: "unknown action: " + r.Action}
	}
	if len(r.Match) == 0 {
		return &ruleErr{msg: "match must be non-empty"}
	}
	if r.Match[0] == tokSingle || r.Match[0] == tokRest {
		return &ruleErr{msg: "wildcards (*, **) are not allowed in the command-name (argv[0]) slot"}
	}
	for i, tok := range r.Match {
		if tok == tokRest && i != len(r.Match)-1 {
			return &ruleErr{msg: "`**` is only valid as the final token of a pattern"}
		}
	}
	return nil
}

type ruleErr struct{ msg string }

func (e *ruleErr) Error() string { return e.msg }
