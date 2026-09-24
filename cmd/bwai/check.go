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
	configFlag := fs.String("config", "", "Path to a config file (overrides ~/.config/bwai/config.json)")
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
		var legacy bool
		configPath, legacy = resolveConfigPath("", home)
		if legacy {
			fmt.Fprintf(os.Stderr, "bwai broker check: %s is deprecated; move it to %s\n", configPath, defaultConfigPath())
		}
	}
	localPath := ""
	if cwd, err := os.Getwd(); err == nil {
		localPath = filepath.Join(cwd, ".bwai.json")
		if localPath == configPath {
			localPath = ""
		}
	}
	cfg, err := loadLayeredConfig(configPath, localPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai broker check: load config: %v\n", err)
		return 2
	}

	return printCheckResult(os.Stdout, cfg.Broker.Rules, argv)
}

// printCheckResult matches argv against rules locally and prints the
// verdict. Used by the host-side `bwai broker check`; the sandbox-side
// `bwai-outside --check` instead receives a verdict over the wire and
// formats it with printCheckVerdict, so both surfaces print identically.
func printCheckResult(w io.Writer, rules []Rule, argv []string) int {
	action, idx := matchRules(rules, argv)
	var rule *Rule
	if idx >= 0 {
		rule = &rules[idx]
	}
	return printCheckVerdict(w, action, idx, rule)
}

// printCheckVerdict formats a match verdict. Kept separate from
// printCheckResult so both the local matcher and the wire client can
// exercise it without juggling stdio.
//
// Exit codes:
//
//	0 — rule matched and action is auto_allow or confirm
//	1 — explicit auto_deny rule matched, or no rule matched (implicit deny)
//	2 — unused here; reserved for usage / I/O errors in the callers
func printCheckVerdict(w io.Writer, action string, idx int, rule *Rule) int {
	if idx < 0 || rule == nil {
		fmt.Fprintln(w, "matched: (none)")
		fmt.Fprintln(w, "result:  AUTO_DENY (implicit)")
		return 1
	}
	ruleJSON, err := json.Marshal(rule)
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
