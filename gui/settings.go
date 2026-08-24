package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings is the state persisted between runs. It exists so a session
// started at login can reproduce the last working configuration without the
// user touching the window.
type Settings struct {
	// StartAtLogin mirrors the OS-level startup entry. The registry (or the
	// XDG entry) remains the source of truth; this is what the UI last set.
	StartAtLogin bool `json:"startAtLogin"`
	// StartProxyOnLaunch starts dispatching as soon as the app opens.
	StartProxyOnLaunch bool `json:"startProxyOnLaunch"`
	// AutoMode keeps load balancers in step with the interfaces that are
	// actually up, re-testing throughput periodically.
	AutoMode bool `json:"autoMode"`
	// LastConfig is the configuration to restore.
	LastConfig ProxyConfig `json:"lastConfig"`
}

// settingsPath returns the settings file location, creating no directories.
func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "go-dispatch-proxy", "settings.json"), nil
}

// loadSettings reads persisted settings. A missing or unreadable file yields
// zero-value settings rather than an error: the app must still start.
func loadSettings() Settings {
	var s Settings

	path, err := settingsPath()
	if err != nil {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}
	}
	return s
}

// saveSettings writes settings atomically, so an interrupted write cannot
// leave a truncated file that fails to parse on the next launch.
func saveSettings(s Settings) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
