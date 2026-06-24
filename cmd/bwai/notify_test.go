package main

import (
	"strings"
	"testing"
)

func TestPendingNotificationContent(t *testing.T) {
	summary, body := pendingNotification(
		[]string{"git", "commit", "-S", "-m", "fix bug"},
		"/home/oscar/source/repos/foo",
	)
	if summary == "" {
		t.Error("summary must be non-empty")
	}
	if !strings.Contains(body, "git commit -S -m fix bug") {
		t.Errorf("body = %q, want it to mention the full command", body)
	}
	if !strings.Contains(body, "/home/oscar/source/repos/foo") {
		t.Errorf("body = %q, want it to mention the project dir", body)
	}
	if !strings.Contains(body, "bwai approve") {
		t.Errorf("body = %q, want it to tell the user how to respond", body)
	}
}

func TestPendingNotificationOmitsEmptyProjectDir(t *testing.T) {
	_, body := pendingNotification([]string{"git", "push"}, "")
	// No project dir → no dangling blank line before the instruction.
	if strings.Contains(body, "\n\n") {
		t.Errorf("body = %q, want no blank line when project dir is empty", body)
	}
}

func TestOobNotifyGate(t *testing.T) {
	cases := []struct {
		name   string
		prompt []string
		want   bool
	}{
		{"default stack", []string{"oob"}, true},
		{"full stack", []string{"tmux", "gui", "oob"}, true},
		{"no oob", []string{"gui"}, false},
		{"empty opts out", []string{}, false},
		{"nil opts out", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BrokerConfig{Prompt: tc.prompt}.oobNotify()
			if got != tc.want {
				t.Errorf("oobNotify(%v) = %v, want %v", tc.prompt, got, tc.want)
			}
		})
	}
}
