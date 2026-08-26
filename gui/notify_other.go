//go:build !windows

package main

import "log"

// Desktop notifications are implemented for Windows only; elsewhere the
// event still reaches the in-app log and the connections table.
func notificationsSupported() bool { return false }

func showNotification(title, body string) {
	log.Println("[INFO]", title+":", body)
}
