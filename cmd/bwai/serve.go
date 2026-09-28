package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// serveConfirmWindow is the period the daemon's confirm cap counts over.
const serveConfirmWindow = time.Hour

// serveRuntimeDir is the daemon's host-only directory: approve.sock and
// the empty directory host commands run in. Under XDG_RUNTIME_DIR so it
// is private to the broker's user and gone at logout or reboot.
func serveRuntimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "bwai-broker")
	}
	return fmt.Sprintf("/tmp/bwai-broker-%d", os.Getuid())
}

// NewServeBroker builds the long-running broker for agents that run as
// other users. The socket's mode is open because the peer-uid check is
// the gate; the directory it lives in keeps everyone else from reaching
// it at all.
func NewServeBroker(cfg BrokerConfig, auditPath string) (*Broker, error) {
	if err := validateServe(cfg.Serve); err != nil {
		return nil, err
	}
	return newBroker(cfg, brokerLayout{
		dir:           serveRuntimeDir(),
		brokerSock:    cfg.Serve.Socket,
		sockMode:      0o666,
		allowedUIDs:   cfg.Serve.AllowedUIDs,
		confirmWindow: serveConfirmWindow,
		daemon:        true,
	}, "", auditPath, cfg.Serve.Roots)
}

// runBrokerServe implements `bwai broker serve [--config PATH]`.
func runBrokerServe(args []string) int {
	fs := flag.NewFlagSet("broker serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configFlag := fs.String("config", "", "Path to a config file (overrides ~/.config/bwai/bwai.json)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path := *configFlag
	if path == "" {
		path = defaultConfigPath()
	}
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai broker serve: %v\n", err)
		return 1
	}
	b, err := NewServeBroker(cfg.Broker, defaultAuditPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai broker serve: %v\n", err)
		return 1
	}
	defer b.Close()

	uids := make([]string, len(cfg.Broker.Serve.AllowedUIDs))
	for i, u := range cfg.Broker.Serve.AllowedUIDs {
		uids[i] = fmt.Sprint(u)
	}
	fmt.Printf("bwai broker: serving %s for uid %s, roots %s\n",
		b.BrokerSocketPath(), strings.Join(uids, ","), strings.Join(cfg.Broker.Serve.Roots, ", "))
	fmt.Printf("bwai broker: approve with `bwai approve --socket %s`\n", b.ApproveSocketPath())

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		_ = b.Close()
	}()
	b.Serve()
	return 0
}
