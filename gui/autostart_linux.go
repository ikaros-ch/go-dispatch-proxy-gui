//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// autostartFileName is the XDG autostart entry read by desktop sessions at
// login (https://specifications.freedesktop.org/autostart-spec/).
const autostartFileName = "go-dispatch-proxy.desktop"

func autostartSupported() bool { return true }

// autostartPath returns ~/.config/autostart/go-dispatch-proxy.desktop.
func autostartPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autostart", autostartFileName), nil
}

// setAutostart writes or removes the XDG autostart entry.
func setAutostart(enabled bool) error {
	path, err := autostartPath()
	if err != nil {
		return err
	}

	if !enabled {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove autostart entry: %w", err)
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine executable path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("could not create autostart directory: %w", err)
	}

	entry := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Go Dispatch Proxy
Exec="%s" -autostart
Terminal=false
X-GNOME-Autostart-enabled=true
`, exe)

	if err := os.WriteFile(path, []byte(entry), 0o644); err != nil {
		return fmt.Errorf("could not write autostart entry: %w", err)
	}
	return nil
}

func getAutostart() (bool, error) {
	path, err := autostartPath()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
