package main

import (
	_ "embed"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed build/windows/icon.ico
var trayIcon []byte

// startTray brings up the system-tray icon with its own message loop.
//
// Wails v2 ships no tray API of its own — only Hide/Show — which is why
// "minimise to tray" used to hide the window with no icon to bring it back
// from. fyne.io/systray pumps its hidden window on a goroutine of its own on
// Windows (nativeStart spawns the GetMessage loop itself), so it coexists
// with the Wails main loop instead of fighting it for the main thread.
func (a *App) startTray() {
	start, _ := systray.RunWithExternalLoop(a.onTrayReady, a.onTrayExit)
	start()
}

func (a *App) onTrayReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("VKMDownload")
	systray.SetTooltip("VKMDownload")

	show := systray.AddMenuItem("显示窗口", "显示主窗口")
	quit := systray.AddMenuItem("退出", "退出程序")

	go func() {
		for range show.ClickedCh {
			if a.ctx != nil {
				runtime.WindowShow(a.ctx)
			}
		}
	}()
	go func() {
		for range quit.ClickedCh {
			systray.Quit()
			if a.ctx != nil {
				runtime.Quit(a.ctx)
			}
		}
	}()

	// Left-click restores the window: the most common tray habit.
	systray.SetOnTapped(func() {
		if a.ctx != nil {
			runtime.WindowShow(a.ctx)
		}
	})
}

func (a *App) onTrayExit() {}

// stopTray removes the icon. Called on shutdown; without it a ghost icon can
// linger until the user hovers it.
func (a *App) stopTray() {
	systray.Quit()
}
