package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// auditEntry is one line in the audit log. Stored as JSONL so each
// decision is independently parseable even if the file is rotated or
// truncated mid-write.
type auditEntry struct {
	TS          string   `json:"ts"`
	Argv        []string `json:"argv"`
	Cwd         string   `json:"cwd,omitempty"`
	MatchedRule int      `json:"matched_rule,omitempty"`
	Decision    string   `json:"decision"`
	ExitCode    *int     `json:"exit_code,omitempty"`
}

// auditLogger serializes writes so concurrent confirm/auto_allow paths
// don't interleave bytes.
type auditLogger struct {
	mu sync.Mutex
	f  *os.File
}

// newAuditLogger opens the audit file for append. If path is empty,
// returns a no-op logger.
func newAuditLogger(path string) (*auditLogger, error) {
	if path == "" {
		return &auditLogger{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &auditLogger{f: f}, nil
}

func (a *auditLogger) write(e auditEntry) {
	if a == nil || a.f == nil {
		return
	}
	e.TS = time.Now().UTC().Format(time.RFC3339Nano)
	a.mu.Lock()
	defer a.mu.Unlock()
	enc := json.NewEncoder(a.f)
	_ = enc.Encode(e)
}

func (a *auditLogger) Close() error {
	if a == nil || a.f == nil {
		return nil
	}
	return a.f.Close()
}

// defaultAuditPath is the location described in the doc:
// ~/.local/state/bwai/broker.log. XDG_STATE_HOME wins if set.
func defaultAuditPath(home string) string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "bwai", "broker.log")
	}
	return filepath.Join(home, ".local", "state", "bwai", "broker.log")
}
