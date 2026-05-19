package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// runBrokerCLI dispatches the second-level word under `bwai broker …`.
// Only `check` exists today; future subcommands (e.g. `list-rules`)
// would land here.
func runBrokerCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "bwai broker: missing subcommand")
		fmt.Fprintln(os.Stderr, "usage: bwai broker check [--config PATH] <argv>...")
		return 2
	}
	switch args[0] {
	case "check":
		return runBrokerCheck(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "bwai broker: unknown subcommand %q\n", args[0])
		return 2
	}
}

// runBrokerCheck implements `bwai broker check [--config PATH] <argv...>`.
// Loads the config, runs the matcher against the supplied argv, and
// prints which rule matched and the resulting action. Useful for
// auditing a non-trivial ruleset without poking the JSON by hand
// (docs/broker.md "Dry-run helper").
//
// Exit codes:
//
//	0 — rule matched and action is auto_allow or confirm
//	1 — explicit auto_deny rule matched, or no rule matched (implicit deny)
//	2 — usage / I/O error
func runBrokerCheck(args []string) int {
	fs := flag.NewFlagSet("broker check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configFlag := fs.String("config", "", "Path to a config file (overrides ~/.bwai.json)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "bwai broker check: missing argv to check")
		fmt.Fprintln(os.Stderr, "usage: bwai broker check [--config PATH] <argv>...")
		return 2
	}

	configPath := *configFlag
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwai broker check: cannot determine home: %v\n", err)
			return 2
		}
		configPath = filepath.Join(home, ".bwai.json")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai broker check: load %s: %v\n", configPath, err)
		return 2
	}

	return printCheckResult(os.Stdout, cfg.Broker.Rules, argv)
}

// printCheckResult formats the matcher output. Kept separate from
// runBrokerCheck so tests can exercise it without juggling stdio.
func printCheckResult(w io.Writer, rules []Rule, argv []string) int {
	action, idx := matchRules(rules, argv)
	if idx < 0 {
		fmt.Fprintln(w, "matched: (none)")
		fmt.Fprintln(w, "result:  AUTO_DENY (implicit)")
		return 1
	}
	ruleJSON, err := json.Marshal(rules[idx])
	if err != nil {
		// Should not happen for an in-memory Rule but be defensive.
		fmt.Fprintf(w, "matched: rules[%d]  (failed to marshal: %v)\n", idx, err)
	} else {
		fmt.Fprintf(w, "matched: rules[%d]  %s\n", idx, string(ruleJSON))
	}
	fmt.Fprintf(w, "result:  %s\n", strings.ToUpper(action))
	if action == ActionAutoDeny {
		return 1
	}
	return 0
}
