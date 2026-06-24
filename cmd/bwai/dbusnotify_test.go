package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNotifier is a desktopNotifier that records Notify/Close calls and
// lets a test drive ActionInvoked events by pushing onto Actions().
type fakeNotifier struct {
	mu       sync.Mutex
	actions  chan notifyAction
	notifies []fakeNotify
	closed   []uint32
	nextID   uint32
	shutOnce sync.Once
}

type fakeNotify struct {
	summary, body, url string
	actions            []string
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{actions: make(chan notifyAction, 16), nextID: 100}
}

func (f *fakeNotifier) Notify(summary, body, url string, actions []string, _ int32) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.notifies = append(f.notifies, fakeNotify{summary, body, url, actions})
	return f.nextID, nil
}

func (f *fakeNotifier) Close(id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id)
	return nil
}

func (f *fakeNotifier) Actions() <-chan notifyAction { return f.actions }

func (f *fakeNotifier) Shutdown() error {
	f.shutOnce.Do(func() { close(f.actions) })
	return nil
}

func (f *fakeNotifier) closedIDs() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint32(nil), f.closed...)
}

// startWebBroker builds a broker in web mode with the given fake
// notifier wired in, then starts it.
func startWebBroker(t *testing.T, fake desktopNotifier, baseURL string, rules []Rule) (*Broker, string) {
	t.Helper()
	projectDir := t.TempDir()
	cfg := BrokerConfig{
		Enabled:          true,
		Prompt:           []string{"web"},
		ApprovalTimeoutS: 5,
		Rules:            rules,
	}
	b := newTestBroker(t, cfg, projectDir)
	b.dbus = fake
	b.baseURL = baseURL
	go b.Serve()
	return b, projectDir
}

// waitNotifID polls until notifyApprover has registered a notification
// id and returns it.
func waitNotifID(t *testing.T, b *Broker) uint32 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		var id uint32
		for k := range b.notifByID {
			id = k
		}
		b.mu.Unlock()
		if id != 0 {
			return id
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no notification id registered")
	return 0
}

func TestDBusAction_ApproveResolves(t *testing.T) {
	fake := newFakeNotifier()
	b, projectDir := startWebBroker(t, fake, "",
		[]Rule{{Match: []string{"echo", "dbus"}, Action: ActionConfirm}})

	resCh := make(chan []brokerFrame, 1)
	go func() {
		resCh <- sendRequest(t, b.BrokerSocketPath(), brokerRequest{
			V: 1, Argv: []string{"echo", "dbus"}, Cwd: projectDir,
		})
	}()

	id := waitNotifID(t, b)
	fake.actions <- notifyAction{ID: id, Key: "approve"}

	select {
	case frames := <-resCh:
		stdout, _, exit := collectStreams(t, frames)
		if strings.TrimSpace(stdout) != "dbus" {
			t.Errorf("stdout = %q, want dbus", stdout)
		}
		if exit == nil || *exit != 0 {
			t.Errorf("exit = %v, want 0", exit)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client never returned after toast approve")
	}

	// The toast is dismissed once the request resolves.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, c := range fake.closedIDs() {
			if c == id {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("notification %d was never closed after resolution", id)
}

func TestDBusAction_DenyResolves(t *testing.T) {
	fake := newFakeNotifier()
	b, projectDir := startWebBroker(t, fake, "",
		[]Rule{{Match: []string{"echo", "no"}, Action: ActionConfirm}})

	resCh := make(chan []brokerFrame, 1)
	go func() {
		resCh <- sendRequest(t, b.BrokerSocketPath(), brokerRequest{
			V: 1, Argv: []string{"echo", "no"}, Cwd: projectDir,
		})
	}()

	id := waitNotifID(t, b)
	fake.actions <- notifyAction{ID: id, Key: "deny"}

	select {
	case frames := <-resCh:
		var denied bool
		for _, fr := range frames {
			if fr.Type == frameTypeDenied && fr.Reason == denyReasonUser {
				denied = true
			}
		}
		if !denied {
			t.Fatalf("expected denied:user, got %+v", frames)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client never returned after toast deny")
	}
}

func TestDBusAction_OpenLaunchesBrowser(t *testing.T) {
	opened := make(chan string, 4)
	orig := browserOpener
	browserOpener = func(url string) { opened <- url }
	t.Cleanup(func() { browserOpener = orig })

	fake := newFakeNotifier()
	b, projectDir := startWebBroker(t, fake, "http://127.0.0.1:54321",
		[]Rule{{Match: []string{"echo", "open"}, Action: ActionConfirm}})

	resCh := make(chan []brokerFrame, 1)
	go func() {
		resCh <- sendRequest(t, b.BrokerSocketPath(), brokerRequest{
			V: 1, Argv: []string{"echo", "open"}, Cwd: projectDir,
		})
	}()

	id := waitNotifID(t, b)
	fake.actions <- notifyAction{ID: id, Key: "open"}

	select {
	case url := <-opened:
		if !strings.HasPrefix(url, "http://127.0.0.1:54321/r/") {
			t.Errorf("opened url = %q, want it to point at the approval page", url)
		}
		if !strings.Contains(url, "?k=") {
			t.Errorf("opened url = %q, want it to carry the token", url)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("open action did not launch the browser")
	}

	// Open doesn't resolve; approve so the client returns before cleanup.
	fake.actions <- notifyAction{ID: id, Key: "approve"}
	select {
	case <-resCh:
	case <-time.After(3 * time.Second):
		t.Fatal("client never returned")
	}
}
