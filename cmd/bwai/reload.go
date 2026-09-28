package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

const configPollInterval = 2 * time.Second

// watchConfig calls reload whenever one of paths changes, or on hup (nil
// for none). It polls rather than using inotify: editors replace files by
// rename, and a stat per interval sees that without re-arming watches.
// reload reports what it applied, or an error, in which case the broker
// is expected to have kept its previous config. report receives one line
// per attempt, and ok says whether it succeeded.
func watchConfig(paths []string, every time.Duration, hup <-chan os.Signal, done <-chan struct{},
	reload func() (string, error), report func(msg string, ok bool)) {
	last := filesStamp(paths)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-hup:
		case <-tick.C:
			if filesStamp(paths) == last {
				continue
			}
		}
		last = filesStamp(paths)
		what, err := reload()
		if err != nil {
			report(fmt.Sprintf("config not reloaded, keeping the previous one: %v", err), false)
			continue
		}
		report(fmt.Sprintf("config reloaded (%s)", what), true)
	}
}

// filesStamp identifies a version of a set of files well enough to
// notice an edit, a replacement, a creation or a deletion.
func filesStamp(paths []string) string {
	parts := make([]string, len(paths))
	for i, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		var ino uint64
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			ino = st.Ino
		}
		parts[i] = fmt.Sprintf("%d/%d/%d", fi.ModTime().UnixNano(), fi.Size(), ino)
	}
	return strings.Join(parts, "|")
}

// reloadSession swaps the per-sandbox broker's policy for requests that
// start after it: rules, push allowlist, protected branches, approval
// timeout. Roots and uids stay: they describe the running sandbox.
func (b *Broker) reloadSession(cfg BrokerConfig) {
	if cfg.ApprovalTimeoutS <= 0 {
		cfg.ApprovalTimeoutS = defaultApprovalTimeoutSec
	}
	b.cfgMu.Lock()
	defer b.cfgMu.Unlock()
	cfg.Web = b.cfg.Web
	cfg.Prompt = b.cfg.Prompt
	b.cfg = cfg
}

// sessionReloader reloads a per-sandbox broker from the global config and
// the project-local layer, under the same trust rule as startup. A local
// file that was applied and has since stopped being trusted (edited
// without a new `bwai trust`) keeps the previous config rather than
// falling back to the global one: a local file may narrow
// push_allowed_urls, so dropping it could widen the session.
func sessionReloader(b *Broker, configPath, localPath string, wasApplied bool, applied func(BrokerConfig)) func() (string, error) {
	return func() (string, error) {
		cfg, state, err := loadProjectConfig(configPath, localPath)
		if err != nil {
			return "", err
		}
		if state == localUntrusted && wasApplied {
			return "", fmt.Errorf("%s changed and is not trusted; review it and run `bwai trust`", localPath)
		}
		wasApplied = state == localApplied
		b.reloadSession(cfg.Broker)
		if applied != nil {
			applied(cfg.Broker)
		}
		return fmt.Sprintf("%d rules", len(cfg.Broker.Rules)), nil
	}
}
