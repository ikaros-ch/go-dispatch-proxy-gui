//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

const (
	mbOK          = 0x00000000
	mbIconError   = 0x00000010
	mbSystemModal = 0x00001000
)

// showFatalDialog pops a native message box so the failure is visible even
// though the process has no console attached.
func showFatalDialog(title, body string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	messageBoxW := user32.NewProc("MessageBoxW")

	titlePtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	bodyPtr, err := syscall.UTF16PtrFromString(body)
	if err != nil {
		return
	}

	_, _, _ = messageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(bodyPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(mbOK|mbIconError|mbSystemModal),
	)
}
