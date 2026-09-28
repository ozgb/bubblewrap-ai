package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// A project-local .bwai.json lives in the tree the agent can write, and
// it is read the next time bwai starts there. Layered unconditionally, an
// agent could write one in this session that widens the next: a
// bwrap_path of its choosing, home_allow of .ssh, auto_allow rules. So a
// local file is only applied when its exact contents were trusted by the
// user on the host with `bwai trust`, and the record of that lives beside
// the global config, which the sandbox cannot write.

// trustStorePath maps absolute .bwai.json paths to the sha256 of the
// contents the user trusted.
func trustStorePath() string {
	return filepath.Join(filepath.Dir(defaultConfigPath()), "trusted.json")
}

func loadTrustStore() (map[string]string, error) {
	data, err := os.ReadFile(trustStorePath())
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	store := map[string]string{}
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("%s: %w", trustStorePath(), err)
	}
	return store, nil
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// localConfigState says what happened to the project-local layer.
type localConfigState int

const (
	localAbsent localConfigState = iota
	localApplied
	localUntrusted
)

// loadProjectConfig is loadLayeredConfig with the local layer gated on
// trust. The local file is read once, and the bytes that were hashed are
// the bytes applied, so swapping the file mid-load gains nothing.
func loadProjectConfig(basePath, localPath string) (Config, localConfigState, error) {
	cfg := defaultConfig()
	if _, err := applyConfigFile(&cfg, basePath, false); err != nil {
		return cfg, localAbsent, err
	}
	state := localAbsent
	if localPath != "" {
		data, err := os.ReadFile(localPath)
		switch {
		case os.IsNotExist(err):
		case err != nil:
			return cfg, localAbsent, err
		default:
			store, serr := loadTrustStore()
			if serr != nil {
				return cfg, localAbsent, serr
			}
			if store[absPath(localPath)] != contentHash(data) {
				state = localUntrusted
				break
			}
			if err := applyConfigData(&cfg, data, localPath, true); err != nil {
				return cfg, localAbsent, err
			}
			state = localApplied
		}
	}
	if err := validateConfig(&cfg); err != nil {
		return cfg, state, err
	}
	return cfg, state, nil
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// untrustedNotice is printed when a local file is skipped.
func untrustedNotice(localPath string) string {
	return fmt.Sprintf("bwai: ignoring %s: not trusted, or changed since it was (review it, then run `bwai trust`)", localPath)
}

// runTrust implements `bwai trust [PATH]`: print the file, then record the
// hash of exactly what was printed.
func runTrust(args []string) int {
	path := ".bwai.json"
	switch len(args) {
	case 0:
	case 1:
		path = args[0]
	default:
		fmt.Fprintln(os.Stderr, "usage: bwai trust [PATH]   (default ./.bwai.json)")
		return 2
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai trust: %v\n", err)
		return 1
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		fmt.Fprintf(os.Stderr, "bwai trust: %s is not a JSON object: %v\n", path, err)
		return 1
	}
	store, err := loadTrustStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai trust: %v\n", err)
		return 1
	}
	key := absPath(path)
	store[key] = contentHash(data)
	out, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwai trust: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(trustStorePath()), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "bwai trust: %v\n", err)
		return 1
	}
	if err := writeFileAtomic(trustStorePath(), append(out, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "bwai trust: %v\n", err)
		return 1
	}
	fmt.Printf("%s\n", data)
	fmt.Printf("bwai: trusted %s (the contents above); any change needs `bwai trust` again\n", key)
	return 0
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".trusted-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
