package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// runApproveCLI implements `bwai approve [--id ID]`. Host-side only.
// Walks the user through pending requests on approve.sock and posts a
// decision frame for each.
//
// The approve socket lives at /tmp/bwai-$PID/approve.sock for the
// currently running bwai parent. We look up the most recent one by
// scanning /tmp; an explicit --socket overrides.
func runApproveCLI(args []string) int {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	idFlag := fs.String("id", "", "Operate on one specific request id (skips the queue walk)")
	sockFlag := fs.String("socket", "", "Path to approve.sock (default: auto-discover from /tmp/bwai-*)")
	if err := fs.Parse(args); err != nil {
		return 64
	}

	sockPath := *sockFlag
	if sockPath == "" {
		discovered, err := discoverApproveSocket()
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwai approve: %v\n", err)
			return 1
		}
		sockPath = discovered
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai approve: connect %s: %v\n", sockPath, err)
		return 1
	}
	defer conn.Close()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)

	if err := enc.Encode(approverRequest{Op: "list"}); err != nil {
		fmt.Fprintf(os.Stderr, "bwai approve: send list: %v\n", err)
		return 1
	}
	var reply approverReply
	if err := dec.Decode(&reply); err != nil {
		fmt.Fprintf(os.Stderr, "bwai approve: read list: %v\n", err)
		return 1
	}
	if reply.Error != "" {
		fmt.Fprintf(os.Stderr, "bwai approve: %s\n", reply.Error)
		return 1
	}
	pending := reply.Pending
	if *idFlag != "" {
		pending = filterByID(pending, *idFlag)
	}
	if len(pending) == 0 {
		fmt.Println("no pending requests.")
		return 0
	}

	fmt.Printf("%d pending request:\n\n", len(pending))
	stdin := bufio.NewReader(os.Stdin)
	for _, p := range pending {
		fmt.Printf("[%s] sandbox wants to run on host\n", p.ID)
		fmt.Printf("  cwd: %s\n", p.Cwd)
		fmt.Printf("  cmd: %s\n", strings.Join(p.Argv, " "))
		fmt.Printf("  age: %dms\n", p.AgeMs)
		fmt.Print("[y]es / [n]o / [a]lways-this-session / [s]kip\n> ")
		line, err := stdin.ReadString('\n')
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwai approve: read input: %v\n", err)
			return 1
		}
		choice := strings.ToLower(strings.TrimSpace(line))
		decision := ""
		switch choice {
		case "y", "yes":
			decision = "approve"
		case "n", "no":
			decision = "deny"
		case "a", "always":
			decision = "always"
		case "s", "skip", "":
			fmt.Println("skipped.")
			continue
		default:
			fmt.Println("unrecognized choice; skipped.")
			continue
		}
		if err := enc.Encode(approverRequest{Op: "decide", ID: p.ID, Decision: decision}); err != nil {
			fmt.Fprintf(os.Stderr, "bwai approve: send decide: %v\n", err)
			return 1
		}
		var dreply approverReply
		if err := dec.Decode(&dreply); err != nil {
			fmt.Fprintf(os.Stderr, "bwai approve: read decide: %v\n", err)
			return 1
		}
		if !dreply.OK {
			fmt.Fprintf(os.Stderr, "bwai approve: %s\n", dreply.Error)
			continue
		}
		fmt.Printf("%sd.\n", decision)
	}
	return 0
}

func filterByID(pending []approverPending, id string) []approverPending {
	out := pending[:0]
	for _, p := range pending {
		if p.ID == id {
			out = append(out, p)
		}
	}
	return out
}

// discoverApproveSocket finds /tmp/bwai-*/approve.sock owned by the
// current user. If there is more than one, the newest wins; bwai
// instances are short-lived enough that the heuristic is good enough
// for MVP. Users who need precision pass --socket.
func discoverApproveSocket() (string, error) {
	matches, err := filepath.Glob("/tmp/bwai-*/approve.sock")
	if err != nil {
		return "", err
	}
	var newest string
	var newestModTime int64
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if info.ModTime().UnixNano() > newestModTime {
			newestModTime = info.ModTime().UnixNano()
			newest = m
		}
	}
	if newest == "" {
		return "", fmt.Errorf("no bwai approve.sock found under /tmp/bwai-*; is bwai running with broker enabled?")
	}
	return newest, nil
}
