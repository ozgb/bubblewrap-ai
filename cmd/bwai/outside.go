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

// runOutsideClient is the entry point used when bwai is invoked as
// `bwai-outside` from inside the sandbox. It dispatches on the first
// arg: introspection flags (--help, --list-rules) talk to the broker
// with the list_rules op; anything else is forwarded as an exec.
//
// Exit code maps the wire-protocol result:
//   - exit frame → that frame's code
//   - denied frame → 126 (per convention; see docs/broker.md follow-up)
//   - connection error → 127
//   - help / list-rules → 0 on success, 127 on transport failure
func runOutsideClient(argv []string) int {
	if len(argv) == 0 {
		return runOutsideHelp()
	}
	switch argv[0] {
	case "--help", "-h", "help":
		return runOutsideHelp()
	case "--list-rules":
		return runOutsideListRules(false)
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
	fmt.Fprintln(os.Stdout, "  bwai-outside --help                this message")
	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "Anything not matched by an allow/confirm rule is denied.")
	fmt.Fprintln(os.Stdout, "Confirm rules prompt the human on the host via `bwai approve`.")
	fmt.Fprintln(os.Stdout, "")
	return runOutsideListRules(true)
}

// runOutsideListRules fetches the rule set from the broker and prints
// it grouped by action. If headerPrinted is true, the caller already
// emitted a top-of-output heading.
func runOutsideListRules(quietHeader bool) int {
	sockPath := os.Getenv("BWAI_BROKER_SOCKET")
	if sockPath == "" {
		fmt.Fprintln(os.Stderr, "bwai-outside: BWAI_BROKER_SOCKET is not set; not running inside a bwai sandbox?")
		return 127
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai-outside: connect: %v\n", err)
		return 127
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(brokerRequest{V: 1, Op: opListRules}); err != nil {
		fmt.Fprintf(os.Stderr, "bwai-outside: send: %v\n", err)
		return 127
	}
	var fr brokerFrame
	if err := json.NewDecoder(conn).Decode(&fr); err != nil {
		fmt.Fprintf(os.Stderr, "bwai-outside: recv: %v\n", err)
		return 127
	}
	if fr.Type != frameTypeRules {
		fmt.Fprintf(os.Stderr, "bwai-outside: unexpected frame %q\n", fr.Type)
		return 127
	}
	if !quietHeader {
		fmt.Println("bwai-outside rules:")
		fmt.Println("")
	}
	printRules(os.Stdout, fr.Rules)
	return 0
}

// printRules formats the rule set for human + LLM consumption. Order:
// rules are kept in their original (config) order so first-match
// precedence is visible, with the longest action label padded for
// alignment. We surface the action upper-cased because matches are
// case-sensitive and the visual distinction helps when scanning.
func printRules(w io.Writer, rules []Rule) {
	if len(rules) == 0 {
		fmt.Fprintln(w, "(no rules configured — everything is denied)")
		return
	}
	const (
		hdrAction = "ACTION"
		hdrRule   = "ARGV PATTERN"
	)
	width := len(hdrAction)
	for _, r := range rules {
		if n := len(strings.ToUpper(r.Action)); n > width {
			width = n
		}
	}
	fmt.Fprintf(w, "  %-*s  %s\n", width, hdrAction, hdrRule)
	fmt.Fprintf(w, "  %-*s  %s\n", width, strings.Repeat("-", width), strings.Repeat("-", len(hdrRule)))
	for _, r := range rules {
		fmt.Fprintf(w, "  %-*s  %s\n", width, strings.ToUpper(r.Action), strings.Join(r.Match, " "))
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "First-match wins. `**` = zero or more trailing args; `*` = exactly one arg.")
	fmt.Fprintln(w, "Use `bwai approve` on the host to clear CONFIRM prompts.")
}

// runOutsideExec is the original exec-forwarding path, unchanged in
// behaviour from the v1 client.
func runOutsideExec(argv []string) int {
	sockPath := os.Getenv("BWAI_BROKER_SOCKET")
	if sockPath == "" {
		fmt.Fprintln(os.Stderr, "bwai-outside: BWAI_BROKER_SOCKET is not set; not running inside a bwai sandbox?")
		return 127
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai-outside: cannot determine cwd: %v\n", err)
		return 127
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai-outside: connect: %v\n", err)
		return 127
	}
	defer conn.Close()

	req := brokerRequest{V: 1, Op: opExec, Argv: argv, Cwd: cwd, StdinInherit: false}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		fmt.Fprintf(os.Stderr, "bwai-outside: send: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "bwai-outside: recv: %v\n", err)
			return 127
		}
		switch fr.Type {
		case frameTypePending:
			if !pendingPrinted {
				fmt.Fprintf(os.Stderr, "bwai-outside: waiting for host approval (id %s)…\n", fr.ID)
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
			fmt.Fprintf(os.Stderr, "bwai-outside: denied (%s); run `bwai-outside --list-rules` to see what's allowed\n", fr.Reason)
			return 126
		default:
			fmt.Fprintf(os.Stderr, "bwai-outside: unknown frame type %q\n", fr.Type)
		}
	}
}
