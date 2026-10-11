package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

// The window opens at a size that fits the task list without scrolling and is
// still usable on a 1366x768 laptop. The minimum is set so the layout cannot be
// squeezed into a state where the progress columns collapse.
const (
	windowWidth  = 1120
	windowHeight = 720
	minWidth     = 880
	minHeight    = 560
)

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "VDownload",
		Width:     windowWidth,
		Height:    windowHeight,
		MinWidth:  minWidth,
		MinHeight: minHeight,

		// Matches the shell background in the front-end theme, so the first
		// frame does not flash a different colour before React paints.
		BackgroundColour: &options.RGBA{R: 13, G: 15, B: 20, A: 1},

		AssetServer: &assetserver.Options{
			Assets: assets,
		},

		OnStartup:     app.startup,
		OnDomReady:    app.domReady,
		OnShutdown:    app.shutdown,
		OnBeforeClose: app.beforeClose,

		Windows: &windows.Options{
			// The dark title bar matches the app shell; otherwise a bright bar
			// sits on top of a dark window.
			Theme: windows.Dark,
			// Keep the window responsive while a large download is running.
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},

		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
