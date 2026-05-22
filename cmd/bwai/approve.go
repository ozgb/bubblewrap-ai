package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// runApproveCLI implements `bwai approve [--id ID] [--socket PATH]`.
// Host-side only. Walks the user through pending requests on every
// running broker's approve.sock and posts decisions back to the broker
// that owns each request.
//
// Each broker lives at /tmp/bwai-$PID/approve.sock. With no --socket,
// we discover every match under /tmp/bwai-* and fan out the `list` op
// across all of them, merging the pending queues into a single walk.
// Decisions are routed back over the per-broker connection so we never
// send a `decide` to the wrong broker.
func runApproveCLI(args []string) int {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	idFlag := fs.String("id", "", "Operate on one specific request id (skips the queue walk)")
	sockFlag := fs.String("socket", "", "Path to a single approve.sock (default: fan out across all /tmp/bwai-*)")
	if err := fs.Parse(args); err != nil {
		return 64
	}

	var sockPaths []string
	if *sockFlag != "" {
		sockPaths = []string{*sockFlag}
	} else {
		discovered, err := discoverApproveSockets()
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwai approve: %v\n", err)
			return 1
		}
		sockPaths = discovered
	}

	// brokerConn keeps the per-broker connection open across list/decide
	// so a decision posts to the broker that originally enqueued the
	// pending request.
	type brokerConn struct {
		sockPath string
		conn     net.Conn
		enc      *json.Encoder
		dec      *json.Decoder
	}
	var conns []*brokerConn
	defer func() {
		for _, bc := range conns {
			_ = bc.conn.Close()
		}
	}()

	type pendingWithSource struct {
		approverPending
		source *brokerConn
	}
	var merged []pendingWithSource

	for _, sp := range sockPaths {
		conn, err := net.Dial("unix", sp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwai approve: connect %s: %v (skipping)\n", sp, err)
			continue
		}
		bc := &brokerConn{
			sockPath: sp,
			conn:     conn,
			enc:      json.NewEncoder(conn),
			dec:      json.NewDecoder(conn),
		}
		if err := bc.enc.Encode(approverRequest{Op: "list"}); err != nil {
			fmt.Fprintf(os.Stderr, "bwai approve: send list to %s: %v\n", sp, err)
			_ = conn.Close()
			continue
		}
		var reply approverReply
		if err := bc.dec.Decode(&reply); err != nil {
			fmt.Fprintf(os.Stderr, "bwai approve: read list from %s: %v\n", sp, err)
			_ = conn.Close()
			continue
		}
		if reply.Error != "" {
			fmt.Fprintf(os.Stderr, "bwai approve: %s: %s\n", sp, reply.Error)
			_ = conn.Close()
			continue
		}
		conns = append(conns, bc)
		for _, p := range reply.Pending {
			merged = append(merged, pendingWithSource{approverPending: p, source: bc})
		}
	}

	if *idFlag != "" {
		filtered := merged[:0]
		for _, p := range merged {
			if p.ID == *idFlag {
				filtered = append(filtered, p)
			}
		}
		merged = filtered
	}
	if len(merged) == 0 {
		fmt.Println("no pending requests.")
		return 0
	}

	fmt.Printf("%d pending request%s across %d broker%s:\n\n",
		len(merged), pluralS(len(merged)), len(conns), pluralS(len(conns)))
	stdin := bufio.NewReader(os.Stdin)
	for _, p := range merged {
		fmt.Printf("[%s] sandbox wants to run on host\n", p.ID)
		if p.ProjectDir != "" {
			fmt.Printf("  project: %s\n", p.ProjectDir)
		}
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
		if err := p.source.enc.Encode(approverRequest{Op: "decide", ID: p.ID, Decision: decision}); err != nil {
			fmt.Fprintf(os.Stderr, "bwai approve: send decide to %s: %v\n", p.source.sockPath, err)
			continue
		}
		var dreply approverReply
		if err := p.source.dec.Decode(&dreply); err != nil {
			fmt.Fprintf(os.Stderr, "bwai approve: read decide from %s: %v\n", p.source.sockPath, err)
			continue
		}
		if !dreply.OK {
			fmt.Fprintf(os.Stderr, "bwai approve: %s\n", dreply.Error)
			continue
		}
		fmt.Printf("%sd.\n", decision)
	}
	return 0
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// discoverApproveSockets returns every /tmp/bwai-*/approve.sock present
// on the host, in lexical (glob) order. Empty result is an error so
// callers can surface a useful message; callers further filter by
// whether the socket actually accepts connections.
func discoverApproveSockets() ([]string, error) {
	matches, err := filepath.Glob("/tmp/bwai-*/approve.sock")
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, errors.New("no bwai approve.sock found under /tmp/bwai-*; is bwai running with broker enabled?")
	}
	return matches, nil
}
