//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

// autostartRunKey is the per-user key Windows reads at sign-in. Using HKCU
// rather than HKLM keeps this out of admin territory: no elevation prompt,
// and the entry only affects the current user.
const autostartRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// autostartValueName identifies our entry among other startup programs.
const autostartValueName = "GoDispatchProxy"

// autostartSupported reports whether this platform can register a startup
// entry at all.
func autostartSupported() bool { return true }

// setAutostart adds or removes the sign-in startup entry. The executable is
// launched with -autostart so it knows to start the proxy unattended.
func setAutostart(enabled bool) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, autostartRunKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("could not open startup registry key: %w", err)
	}
	defer key.Close()

	if !enabled {
		err := key.DeleteValue(autostartValueName)
		// Already absent is the desired end state, not a failure.
		if err != nil && err != registry.ErrNotExist {
			return fmt.Errorf("could not remove startup entry: %w", err)
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine executable path: %w", err)
	}

	// Quoted so a path containing spaces survives command line parsing.
	command := fmt.Sprintf(`"%s" -autostart`, exe)
	if err := key.SetStringValue(autostartValueName, command); err != nil {
		return fmt.Errorf("could not write startup entry: %w", err)
	}
	return nil
}

// getAutostart reports whether the startup entry is currently registered.
func getAutostart() (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.QUERY_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return false, nil
		}
		return false, err
	}
	defer key.Close()

	if _, _, err := key.GetStringValue(autostartValueName); err != nil {
		if err == registry.ErrNotExist {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
