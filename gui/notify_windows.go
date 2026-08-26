//go:build windows

package main

import (
	"log"
	"sync"

	"git.sr.ht/~jackmordaunt/go-toast/v2"
)

// notifyAppID identifies this application to the Windows notification
// centre, so its toasts are grouped and can be managed in Settings.
const notifyAppID = "com.ikaros-ch.go-dispatch-proxy"

// notifyGUID is the fixed activation identity Windows associates with our
// notifications. It must stay constant or previously shown toasts lose
// their association with the app.
const notifyGUID = "6f1f4a02-7c1e-4a1e-9f5d-2b7c0a4e51d3"

var registerNotifications sync.Once

func notificationsSupported() bool { return true }

// showNotification raises a Windows toast. Failures are logged rather than
// returned: a missing notification must never interfere with proxying.
func showNotification(title, body string) {
	registerNotifications.Do(func() {
		// Registering app data is what lets the toast show a proper name
		// instead of the executable path.
		if err := toast.SetAppData(toast.AppData{
			AppID: notifyAppID,
			GUID:  notifyGUID,
		}); err != nil {
			log.Println("[DEBUG] Could not register notification metadata:", err)
		}
	})

	notification := toast.Notification{
		AppID: notifyAppID,
		Title: title,
		Body:  body,
	}

	if err := notification.Push(); err != nil {
		log.Println("[DEBUG] Could not show notification:", err)
	}
}
