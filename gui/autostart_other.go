//go:build !windows && !linux

package main

import "errors"

// The remaining platforms have no supported startup mechanism here, so the
// UI hides the option rather than offering something that cannot work.
func autostartSupported() bool { return false }

func setAutostart(bool) error {
	return errors.New("starting at login is not supported on this platform")
}

func getAutostart() (bool, error) { return false, nil }
