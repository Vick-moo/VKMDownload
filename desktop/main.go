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

// The window opens wide enough for six task columns plus the sidebar without
// the action buttons being pushed off the right edge. The minimum keeps the
// columns from colliding; below the default width the list scrolls sideways
// rather than dropping controls.
const (
	windowWidth  = 1280
	windowHeight = 800
	minWidth     = 1000
	minHeight    = 600
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
