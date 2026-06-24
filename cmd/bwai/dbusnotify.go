package main

import (
	"context"
	"os/exec"

	"github.com/godbus/dbus/v5"
)

const (
	notifyBusName    = "org.freedesktop.Notifications"
	notifyObjectPath = "/org/freedesktop/Notifications"
	notifyInterface  = "org.freedesktop.Notifications"
)

// notifyAction is a button/body click reported by the notification
// daemon: which notification (ID) and which action key fired.
type notifyAction struct {
	ID  uint32
	Key string
}

// desktopNotifier abstracts the D-Bus notification surface so broker
// logic stays unit-testable against a fake. The production
// implementation talks to org.freedesktop.Notifications on the session
// bus.
type desktopNotifier interface {
	// Notify posts a notification and returns the daemon-assigned id.
	// actions is the freedesktop [key, label, key, label, …] list. The
	// url is also folded into the body by the caller as an <a href>
	// fallback for daemons that don't render action buttons.
	Notify(summary, body, url string, actions []string, expireMs int32) (uint32, error)
	// Close dismisses a previously-posted notification by id.
	Close(id uint32) error
	// Actions streams action-invoked events; closed by Shutdown.
	Actions() <-chan notifyAction
	// Shutdown tears down the bus connection and closes Actions().
	Shutdown() error
}

// dbusNotifier is the godbus-backed desktopNotifier.
type dbusNotifier struct {
	conn    *dbus.Conn
	obj     dbus.BusObject
	signals chan *dbus.Signal
	actions chan notifyAction
}

// newDBUSNotifier connects to the session bus and subscribes to
// ActionInvoked signals. Returns an error when no session bus is
// reachable (headless / DBUS_SESSION_BUS_ADDRESS unset); callers degrade
// to the oob nudge rather than treating that as fatal.
func newDBUSNotifier() (*dbusNotifier, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	n := &dbusNotifier{
		conn:    conn,
		obj:     conn.Object(notifyBusName, notifyObjectPath),
		signals: make(chan *dbus.Signal, 16),
		actions: make(chan notifyAction, 16),
	}
	// Best-effort match rule. If it fails we still register the channel;
	// the toast buttons go inert but the linked web page still works.
	_ = conn.AddMatchSignal(
		dbus.WithMatchObjectPath(notifyObjectPath),
		dbus.WithMatchInterface(notifyInterface),
		dbus.WithMatchMember("ActionInvoked"),
	)
	conn.Signal(n.signals)
	go n.forward()
	return n, nil
}

// forward turns raw ActionInvoked signals into notifyAction values. It
// ends when the bus connection closes (Shutdown closes the signal
// channel), then closes actions so the broker's pump returns.
func (n *dbusNotifier) forward() {
	defer close(n.actions)
	for sig := range n.signals {
		if sig.Name != notifyInterface+".ActionInvoked" || len(sig.Body) < 2 {
			continue
		}
		id, ok := sig.Body[0].(uint32)
		if !ok {
			continue
		}
		key, ok := sig.Body[1].(string)
		if !ok {
			continue
		}
		n.actions <- notifyAction{ID: id, Key: key}
	}
}

func (n *dbusNotifier) Notify(summary, body, url string, actions []string, expireMs int32) (uint32, error) {
	_ = url // already embedded in body by the caller; reserved for hints.
	hints := map[string]dbus.Variant{
		"urgency": dbus.MakeVariant(byte(1)), // normal
	}
	call := n.obj.Call(notifyInterface+".Notify", 0,
		"bwai",            // app_name
		uint32(0),         // replaces_id
		"dialog-password", // app_icon
		summary,           // summary
		body,              // body
		actions,           // actions
		hints,             // hints
		expireMs,          // expire_timeout (ms; <0 = daemon default)
	)
	if call.Err != nil {
		return 0, call.Err
	}
	var id uint32
	if err := call.Store(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (n *dbusNotifier) Close(id uint32) error {
	if id == 0 {
		return nil
	}
	return n.obj.Call(notifyInterface+".CloseNotification", 0, id).Err
}

func (n *dbusNotifier) Actions() <-chan notifyAction { return n.actions }

func (n *dbusNotifier) Shutdown() error { return n.conn.Close() }

// pumpNotifications routes desktop-notification actions to decisions.
// The session bus is host-only — DBUS_SESSION_BUS_ADDRESS is not in
// env_allow and /run is a tmpfs inside the sandbox — so ActionInvoked
// events are trusted and resolve without a token, unlike the web path.
func (b *Broker) pumpNotifications() {
	for a := range b.dbus.Actions() {
		switch a.Key {
		case "approve", "deny":
			b.resolveByNotification(a.ID, a.Key)
		case "default", "open":
			b.openByNotification(a.ID)
		}
	}
}

// resolveByNotification delivers a trusted toast decision to the request
// the notification id maps to. No-op if the id is unknown (already
// resolved and unregistered).
func (b *Broker) resolveByNotification(notifID uint32, decision string) {
	b.mu.Lock()
	p := b.notifByID[notifID]
	b.mu.Unlock()
	if p != nil {
		p.resolve(decision)
	}
}

// openByNotification launches the request's approval page in the user's
// browser (toast body or "Open page" button).
func (b *Broker) openByNotification(notifID uint32) {
	b.mu.Lock()
	p := b.notifByID[notifID]
	b.mu.Unlock()
	if p == nil {
		return
	}
	if url := b.urlFor(p); url != "" {
		browserOpener(url)
	}
}

// browserOpener launches a URL in the user's browser. Package var so
// tests can capture the URL instead of spawning xdg-open.
var browserOpener = openXDG

// openXDG opens url via xdg-open, best-effort and non-blocking. No-ops
// when xdg-open is absent; the timeout guards against a wedged handler
// stalling nothing in particular (it runs detached anyway).
func openXDG(url string) {
	path, err := exec.LookPath("xdg-open")
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		_ = exec.CommandContext(ctx, path, url).Run()
	}()
}
