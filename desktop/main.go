package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"

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

// webviewDataDir is where WebView2 keeps its cache and local storage.
//
// UserCacheDir is %LOCALAPPDATA% on Windows: per-machine, never roamed, and
// removed by the usual cleanup tools. Falling back to the temp directory keeps
// the app starting even if that call fails.
func webviewDataDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, appDirName, "webview2")
}

func main() {
	// The update helper is this same executable, started from its own backup
	// copy. It must never reach wails.Run: it needs no window, and creating the
	// single-instance lock or the WebView2 runtime from a process that is about
	// to be replaced would be pointless at best.
	if args, ok := parseHelperArgs(os.Args[1:]); ok {
		// The helper exists to replace one specific executable with another, and
		// it is started from a backup copy of itself. Without this check the
		// program is a general-purpose "replace any file with any other file"
		// tool for anyone who can pass it arguments — it would happily overwrite
		// a document with an arbitrary executable. The check lives here rather
		// than in runHelper because this is the only place the real running
		// image is known (tests drive runHelper through an injected sys).
		if self, err := os.Executable(); err == nil && args.Bak != "" && !samePathText(self, args.Bak) {
			fmt.Fprintf(os.Stderr, "refusing to run as an update helper: --bak %q is not this process (%q)\n", args.Bak, self)
			os.Exit(2)
		}
		os.Exit(runHelper(args, realUpdateSys{
			logPath: filepath.Join(filepath.Dir(args.State), "vkmdownload.log"),
		}))
	}

	app := NewApp()
	app.updateToken = parseHandshakeToken(os.Args[1:])

	err := wails.Run(&options.App{
		Title:     "VKMDownload",
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

		// A second launch wakes the running instance instead of starting a
		// new one: without this every double-click is a fully independent
		// process with its own tray icon, loopback port and task list.
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "vkmdownload-single-instance",
			OnSecondInstanceLaunch: app.onSecondInstance,
		},

		Windows: &windows.Options{
			// The dark title bar matches the app shell; otherwise a bright bar
			// sits on top of a dark window.
			Theme: windows.Dark,
			// Keep the window responsive while a large download is running.
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,

			// WebView2's cache lives under the local app data directory, named
			// explicitly. The default put it under Roaming (keyed by the exe
			// name), which is wrong on two counts: a browser cache has no
			// business being copied between machines by a roaming profile, and a
			// stale folder there could not be recreated after it was removed,
			// leaving the app unable to open a window at all.
			WebviewUserDataPath: webviewDataDir(),
		},

		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
