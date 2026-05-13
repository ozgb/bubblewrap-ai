package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
)

// runOutsideClient is the entry point used when bwai is invoked as
// `bwai-outside` from inside the sandbox. It connects to the broker
// socket (BWAI_BROKER_SOCKET), forwards argv, and replays reply frames
// to stdout/stderr.
//
// Exit code maps the wire-protocol result:
//   - exit frame → that frame's code
//   - denied frame → 126 (per convention; see docs/broker.md follow-up)
//   - connection error → 127
func runOutsideClient(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "bwai-outside: missing command")
		return 64
	}
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

	req := brokerRequest{V: 1, Argv: argv, Cwd: cwd, StdinInherit: false}
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
			fmt.Fprintf(os.Stderr, "bwai-outside: denied (%s)\n", fr.Reason)
			return 126
		default:
			fmt.Fprintf(os.Stderr, "bwai-outside: unknown frame type %q\n", fr.Type)
		}
	}
}
