package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// reportFatal records a startup failure somewhere the user can actually see
// it. A release build is compiled for the GUI subsystem, so anything written
// to stdout/stderr is discarded and the process just vanishes; this writes a
// log file and, where supported, shows a native dialog.
func reportFatal(msg string) {
	path := fatalLogPath()
	if path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		line := fmt.Sprintf("[%s] %s\n", time.Now().Format(time.RFC3339), msg)
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		}
	}

	detail := msg
	if path != "" {
		detail = fmt.Sprintf("%s\n\nDetails written to:\n%s", msg, path)
	}
	showFatalDialog("Go Dispatch Proxy failed to start", detail)
}

// fatalLogPath returns the file startup errors are appended to, or "" if no
// suitable location could be determined.
func fatalLogPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "go-dispatch-proxy", "startup-error.log")
}
