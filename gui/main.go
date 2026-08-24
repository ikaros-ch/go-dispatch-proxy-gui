package main

import (
	"context"
	"embed"
	"flag"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

// autostart is set by the startup entry registered with the OS, telling this
// instance it was launched at sign-in and should restore the saved proxy.
var autostart = flag.Bool("autostart", false, "started automatically at login; restore the saved configuration")

// launchedForAutostart reports whether this process was started by the OS
// startup entry.
func launchedForAutostart() bool { return *autostart }

func main() {
	flag.Parse()

	// Create an instance of the app structure
	app := NewApp()

	// Create application with options
	err := wails.Run(&options.App{
		Title:  "Go Dispatch Proxy",
		Width:  1024,
		Height: 768,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        app.startup,
		OnShutdown: func(ctx context.Context) {
			app.StopProxy()
		},
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		reportFatal(err.Error())
	}
}
