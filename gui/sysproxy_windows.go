//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// internetSettingsKey holds the per-user WinINET proxy configuration that
// Settings > Network & Internet > Proxy edits, and that browsers and most
// desktop applications read.
const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// defaultBypass keeps local addresses off the proxy. Without <local>, even
// intranet hosts would be sent through the dispatcher.
const defaultBypass = "localhost;127.*;10.*;172.16.*;172.17.*;172.18.*;172.19.*;172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;172.27.*;172.28.*;172.29.*;172.30.*;172.31.*;192.168.*;<local>"

// WinINET option codes for refreshing live settings.
const (
	internetOptionSettingsChanged = 39
	internetOptionRefresh         = 37
)

func systemProxySupported() bool { return true }

// systemProxyState captures the settings in effect before we changed them,
// so they can be put back exactly as they were.
type systemProxyState struct {
	Enable   uint64 `json:"enable"`
	Server   string `json:"server"`
	Override string `json:"override"`
	// HadServer and HadOverride record whether the values existed at all;
	// restoring must delete them again if they did not.
	HadServer   bool `json:"hadServer"`
	HadOverride bool `json:"hadOverride"`
}

// readSystemProxy captures the current configuration.
func readSystemProxy() (systemProxyState, error) {
	var state systemProxyState

	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return state, fmt.Errorf("could not read proxy settings: %w", err)
	}
	defer key.Close()

	if enable, _, err := key.GetIntegerValue("ProxyEnable"); err == nil {
		state.Enable = enable
	}
	if server, _, err := key.GetStringValue("ProxyServer"); err == nil {
		state.Server = server
		state.HadServer = true
	}
	if override, _, err := key.GetStringValue("ProxyOverride"); err == nil {
		state.Override = override
		state.HadOverride = true
	}
	return state, nil
}

// applySystemProxy points the system at address ("host:port").
func applySystemProxy(address string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("could not open proxy settings: %w", err)
	}
	defer key.Close()

	if err := key.SetStringValue("ProxyServer", address); err != nil {
		return fmt.Errorf("could not set proxy address: %w", err)
	}
	if err := key.SetStringValue("ProxyOverride", defaultBypass); err != nil {
		return fmt.Errorf("could not set proxy bypass list: %w", err)
	}
	if err := key.SetDWordValue("ProxyEnable", 1); err != nil {
		return fmt.Errorf("could not enable the proxy: %w", err)
	}

	notifyProxyChanged()
	return nil
}

// restoreSystemProxy puts back a previously captured configuration.
func restoreSystemProxy(state systemProxyState) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("could not open proxy settings: %w", err)
	}
	defer key.Close()

	// Disable first, so a failure partway through cannot leave the system
	// pointing at a proxy that is no longer listening.
	if err := key.SetDWordValue("ProxyEnable", uint32(state.Enable)); err != nil {
		return fmt.Errorf("could not restore proxy state: %w", err)
	}

	if state.HadServer {
		if err := key.SetStringValue("ProxyServer", state.Server); err != nil {
			return fmt.Errorf("could not restore proxy address: %w", err)
		}
	} else if err := key.DeleteValue("ProxyServer"); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("could not clear proxy address: %w", err)
	}

	if state.HadOverride {
		if err := key.SetStringValue("ProxyOverride", state.Override); err != nil {
			return fmt.Errorf("could not restore proxy bypass list: %w", err)
		}
	} else if err := key.DeleteValue("ProxyOverride"); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("could not clear proxy bypass list: %w", err)
	}

	notifyProxyChanged()
	return nil
}

// notifyProxyChanged tells running applications to re-read the settings.
// Without this they keep using the previous configuration until restarted.
func notifyProxyChanged() {
	wininet := syscall.NewLazyDLL("wininet.dll")
	internetSetOption := wininet.NewProc("InternetSetOptionW")

	// Both calls are advisory; a failure only means some applications pick
	// the change up later, so their results are deliberately ignored.
	internetSetOption.Call(0, internetOptionSettingsChanged, uintptr(unsafe.Pointer(nil)), 0)
	internetSetOption.Call(0, internetOptionRefresh, uintptr(unsafe.Pointer(nil)), 0)
}
