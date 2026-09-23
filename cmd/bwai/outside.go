package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

// outsideProg is the name this client reports errors under. It is set
// from argv[0] in main, so a request made through the `git-safe` persona
// is reported as `git-safe` rather than as the `bwai-outside` client it
// happens to share code with. Defaults to the original name so direct
// calls (tests) and the bwai-outside persona are unchanged.
var outsideProg = "bwai-outside"

// runOutsideClient is the entry point used when bwai is invoked as
// `bwai-outside` from inside the sandbox. It dispatches on the first
// arg: introspection flags (--help, --list-rules, --check) talk to the
// broker with the list_rules/check ops; anything else is forwarded as
// an exec.
//
// Exit code maps the wire-protocol result:
//   - exit frame → that frame's code
//   - denied frame → 126 (per convention; see docs/broker.md follow-up)
//   - connection error → 127
//   - help / list-rules / check → 0 on success, 127 on transport failure
func runOutsideClient(argv []string) int {
	if len(argv) == 0 {
		return runOutsideHelp()
	}
	switch argv[0] {
	case "--help", "-h", "help":
		return runOutsideHelp()
	case "--list-rules":
		return runOutsideListRules(false)
	case "--check":
		return runOutsideCheck(argv[1:])
	}
	return runOutsideExec(argv)
}

// runOutsideHelp prints usage plus the current rule list. Intended as
// the first thing an agent sees if it tries `bwai-outside` blind.
func runOutsideHelp() int {
	fmt.Fprintln(os.Stdout, "bwai-outside — run a host command from inside the bwai sandbox")
	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "Usage:")
	fmt.Fprintln(os.Stdout, "  bwai-outside <command> [args...]   run on host (subject to broker rules)")
	fmt.Fprintln(os.Stdout, "  bwai-outside --list-rules          print the rules and exit")
	fmt.Fprintln(os.Stdout, "  bwai-outside --check <cmd> [args]  dry-run: show which rule would match, run nothing")
	fmt.Fprintln(os.Stdout, "  bwai-outside --help                this message")
	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "Anything not matched by an allow/confirm rule is denied.")
	fmt.Fprintln(os.Stdout, "Confirm rules prompt the human on the host via `bwai approve`.")
	fmt.Fprintln(os.Stdout, "")
	return runOutsideListRules(true)
}

// brokerDial connects to the broker socket. The int is the exit code to
// return when dialing failed (nil conn).
func brokerDial() (net.Conn, int) {
	sockPath := os.Getenv("BWAI_BROKER_SOCKET")
	if sockPath == "" {
		fmt.Fprintln(os.Stderr, outsideProg+": BWAI_BROKER_SOCKET is not set; not running inside a bwai sandbox?")
		return nil, 127
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, outsideProg+": connect: %v\n", err)
		return nil, 127
	}
	return conn, 0
}

// runOutsideListRules fetches the rule set from the broker and prints
// it grouped by action. If headerPrinted is true, the caller already
// emitted a top-of-output heading.
func runOutsideListRules(quietHeader bool) int {
	conn, code := brokerDial()
	if conn == nil {
		return code
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(brokerRequest{V: 1, Op: opListRules}); err != nil {
		fmt.Fprintf(os.Stderr, outsideProg+": send: %v\n", err)
		return 127
	}
	var fr brokerFrame
	if err := json.NewDecoder(conn).Decode(&fr); err != nil {
		fmt.Fprintf(os.Stderr, outsideProg+": recv: %v\n", err)
		return 127
	}
	if fr.Type != frameTypeRules {
		fmt.Fprintf(os.Stderr, outsideProg+": unexpected frame %q\n", fr.Type)
		return 127
	}
	if !quietHeader {
		fmt.Println("bwai-outside rules:")
		fmt.Println("")
	}
	printRules(os.Stdout, fr.Rules)
	return 0
}

// runOutsideCheck asks the broker which rule would apply to argv
// without executing it — the sandbox-side twin of `bwai broker check`,
// with the same output shape and exit codes (0 allow/confirm, 1 deny,
// 2 usage). Agents use it to settle "which rule applies?" empirically
// instead of re-deriving first-match precedence from the rule list.
func runOutsideCheck(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, outsideProg+": --check needs a command to check")
		fmt.Fprintf(os.Stderr, "usage: %s --check <command> [args...]\n", outsideProg)
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	conn, code := brokerDial()
	if conn == nil {
		return code
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(brokerRequest{V: 1, Op: opCheck, Argv: argv, Cwd: cwd}); err != nil {
		fmt.Fprintf(os.Stderr, outsideProg+": send: %v\n", err)
		return 127
	}
	var fr brokerFrame
	if err := json.NewDecoder(conn).Decode(&fr); err != nil {
		fmt.Fprintf(os.Stderr, outsideProg+": recv: %v\n", err)
		return 127
	}
	switch fr.Type {
	case frameTypeCheck:
	case frameTypeDenied:
		fmt.Fprintf(os.Stderr, outsideProg+": check refused (%s)\n", fr.Reason)
		return 126
	default:
		fmt.Fprintf(os.Stderr, outsideProg+": unexpected frame %q\n", fr.Type)
		return 127
	}
	m := fr.Matched
	if m == nil {
		m = &matchedRule{Idx: -1, Action: ActionAutoDeny}
	}
	return printCheckVerdict(os.Stdout, m.Action, m.Idx, m.Rule)
}

// printRules formats the rule set for human + LLM consumption. Rules
// are kept in their original (config) order so first-match precedence
// is visible, each row numbered rules[N] so denials and --check output
// (which cite the same indices) can be cross-referenced against this
// view. We surface the action upper-cased because matches are
// case-sensitive and the visual distinction helps when scanning.
func printRules(w io.Writer, rules []Rule) {
	if len(rules) == 0 {
		fmt.Fprintln(w, "(no rules configured — everything is denied)")
		return
	}
	const (
		hdrAction = "ACTION"
		hdrIndex  = "RULE"
		hdrRule   = "ARGV PATTERN"
	)
	width := len(hdrAction)
	for _, r := range rules {
		if n := len(strings.ToUpper(r.Action)); n > width {
			width = n
		}
	}
	idxWidth := len(hdrIndex)
	for i := range rules {
		if n := len(fmt.Sprintf("rules[%d]", i)); n > idxWidth {
			idxWidth = n
		}
	}
	fmt.Fprintf(w, "  %-*s  %-*s  %s\n", width, hdrAction, idxWidth, hdrIndex, hdrRule)
	fmt.Fprintf(w, "  %s  %s  %s\n", strings.Repeat("-", width), strings.Repeat("-", idxWidth), strings.Repeat("-", len(hdrRule)))
	for i, r := range rules {
		fmt.Fprintf(w, "  %-*s  %-*s  %s\n", width, strings.ToUpper(r.Action), idxWidth, fmt.Sprintf("rules[%d]", i), strings.Join(r.Match, " "))
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "First-match wins. `**` = zero or more trailing args; `*` = exactly one arg.")
	fmt.Fprintln(w, "Patterns match argv token-by-token, so a flag shifts every later token:")
	fmt.Fprintln(w, "  `gh issue -R org/repo create` does NOT match `gh issue create **` —")
	fmt.Fprintln(w, "  its 2nd token is `-R`, not `create` — it lands on a narrow rule like")
	fmt.Fprintln(w, "  `gh issue -R org/repo create **` instead (typically the AUTO_ALLOW one).")
	fmt.Fprintln(w, "Not sure which rule fires? `bwai-outside --check <cmd> [args...]` asks the broker dry-run.")
	fmt.Fprintln(w, "Use `bwai approve` on the host to clear CONFIRM prompts.")
}

// runOutsideExec is the original exec-forwarding path, unchanged in
// behaviour from the v1 client.
func runOutsideExec(argv []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, outsideProg+": cannot determine cwd: %v\n", err)
		return 127
	}

	conn, code := brokerDial()
	if conn == nil {
		return code
	}
	defer conn.Close()

	req := brokerRequest{V: 1, Op: opExec, Argv: argv, Cwd: cwd, StdinInherit: false}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		fmt.Fprintf(os.Stderr, outsideProg+": send: %v\n", err)
		return 127
	}

	dec := json.NewDecoder(conn)
	pendingPrinted := false
	for {
		var fr brokerFrame
		if err := dec.Decode(&fr); err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			fmt.Fprintf(os.Stderr, outsideProg+": recv: %v\n", err)
			return 127
		}
		switch fr.Type {
		case frameTypePending:
			if !pendingPrinted {
				fmt.Fprintf(os.Stderr, "%s: waiting for host approval (id %s)%s\n",
					outsideProg, fr.ID, matchedRuleHint(fr.Matched))
				pendingPrinted = true
			}
		case frameTypeStdout:
			_, _ = io.WriteString(os.Stdout, fr.Data)
		case frameTypeStderr:
			_, _ = io.WriteString(os.Stderr, fr.Data)
		case frameTypeExit:
			if fr.Code == nil {
				return 0
			}
			return *fr.Code
		case frameTypeDenied:
			fmt.Fprintf(os.Stderr, "%s: denied (%s)%s; run `bwai-outside --list-rules` to see what's allowed\n",
				outsideProg, fr.Reason, matchedRuleHint(fr.Matched))
			return 126
		default:
			fmt.Fprintf(os.Stderr, outsideProg+": unknown frame type %q\n", fr.Type)
		}
	}
}

// matchedRuleHint renders the matched-rule detail the broker attaches to
// pending and denied frames: " — rules[12] AUTO_DENY `gh secret **`".
// Empty when no rule matched (implicit deny) or the frame carries no
// verdict (older broker, invalid-request denies).
func matchedRuleHint(m *matchedRule) string {
	if m == nil || m.Idx < 0 || m.Rule == nil {
		return ""
	}
	return fmt.Sprintf(" — rules[%d] %s `%s`", m.Idx, strings.ToUpper(m.Action), strings.Join(m.Rule.Match, " "))
}
