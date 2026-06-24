package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// notifier fires a best-effort desktop notification with the given
// summary and body. It's a package var so tests can swap in a capture
// stub; production uses notifySend.
var notifier = notifySend

// notifyTimeout caps how long we wait on notify-send before giving up.
// A wedged notification daemon must never stall an approval.
const notifyTimeout = 10 * time.Second

// notifySend posts a desktop notification via notify-send (libnotify).
// It runs host-side inside the bwai parent process, which inherits the
// user's session env (DISPLAY / WAYLAND_DISPLAY /
// DBUS_SESSION_BUS_ADDRESS), so the notification reaches their desktop.
//
// Best-effort by design: it no-ops silently when notify-send isn't on
// PATH and never blocks the caller. The actual send happens in a
// goroutine with a timeout so a missing or hung notification daemon
// can't stall the broker's approval path.
func notifySend(summary, body string) {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		// Separate-arg flag form for compatibility with older
		// notify-send. dialog-password is a standard freedesktop icon
		// name; absent themes simply render without an icon.
		cmd := exec.CommandContext(ctx, path,
			"--app-name", "bwai",
			"--icon", "dialog-password",
			summary, body)
		_ = cmd.Run()
	}()
}

// pendingNotification renders the summary and body for a confirm request
// that's waiting on the user. Split from the firing path so it can be
// unit-tested without a notification daemon.
func pendingNotification(argv []string, projectDir string) (summary, body string) {
	summary = "bwai: approval needed"
	var b strings.Builder
	b.WriteString(strings.Join(argv, " "))
	if projectDir != "" {
		b.WriteString("\n")
		b.WriteString(projectDir)
	}
	b.WriteString("\nRun `bwai approve` to respond.")
	return summary, b.String()
}

// webNotification renders the summary and body for the rich D-Bus
// notification used by "web" mode. The body names the command and
// project like the oob nudge, plus a body-hyperlinks <a href> to the
// token-protected approval page so daemons that don't render action
// buttons still expose a clickable link.
func webNotification(argv []string, projectDir, url string) (summary, body string) {
	summary = "bwai: approval needed"
	var b strings.Builder
	b.WriteString(strings.Join(argv, " "))
	if projectDir != "" {
		b.WriteString("\n")
		b.WriteString(projectDir)
	}
	if url != "" {
		b.WriteString("\n")
		b.WriteString(`<a href="`)
		b.WriteString(url)
		b.WriteString(`">Open approval page</a>`)
	}
	return summary, b.String()
}

// oobNotify reports whether the out-of-band approver's desktop
// notification is enabled — i.e. "oob" is in the configured prompt
// stack. The default config includes it; setting broker.prompt without
// "oob" opts out.
func (c BrokerConfig) oobNotify() bool {
	for _, m := range c.Prompt {
		if m == "oob" {
			return true
		}
	}
	return false
}
