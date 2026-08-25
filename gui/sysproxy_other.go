//go:build !windows

package main

import "errors"

// Configuring the OS-wide proxy is implemented for Windows only. Elsewhere
// the UI hides the option and the HTTP proxy can still be entered manually
// in the desktop environment's network settings.
type systemProxyState struct {
	Enable      uint64 `json:"enable"`
	Server      string `json:"server"`
	Override    string `json:"override"`
	HadServer   bool   `json:"hadServer"`
	HadOverride bool   `json:"hadOverride"`
}

func systemProxySupported() bool { return false }

var errSystemProxyUnsupported = errors.New("setting the system proxy is not supported on this platform")

func readSystemProxy() (systemProxyState, error) { return systemProxyState{}, nil }

func applySystemProxy(string) error { return errSystemProxyUnsupported }

func restoreSystemProxy(systemProxyState) error { return nil }
