package main

import (
	_ "embed"
	"sync"
	"time"

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

// trayStateRefresh is how often the menu repaints its enabled/checked state.
//
// systray has no "menu opened" event, so a slow poll is the way "暂停全部" goes
// grey once nothing is running. Two seconds is far below human patience and far
// above any noticeable cost (the counters are one pass over the task table).
const trayStateRefresh = 2 * time.Second

// trayExitCh closes when the tray icon goes away, stopping the repaint goroutine.
// One tray per process, so a package-level channel is enough.
var (
	trayExitOnce sync.Once
	trayExitCh   = make(chan struct{})
)

// traySpeedPreset is one entry in the 限速 submenu. KB is the cap in KiB/s, 0
// meaning no limit — the same unit `speedLimitKb` uses everywhere.
type traySpeedPreset struct {
	Label string
	KB    int
}

// traySpeedPresets lists the presets in menu order. The first is "no limit", so
// choosing it clears a cap rather than setting a very large one.
func traySpeedPresets() []traySpeedPreset {
	return []traySpeedPreset{
		{Label: "不限速", KB: 0},
		{Label: "1 MB/s", KB: 1024},
		{Label: "5 MB/s", KB: 5 * 1024},
		{Label: "10 MB/s", KB: 10 * 1024},
	}
}

// trayCheckedSpeedIndex is the index of the preset matching speedKB, or -1 when
// the cap is a custom value typed into the settings page (none ticked then).
func trayCheckedSpeedIndex(speedKB int) int {
	for i, preset := range traySpeedPresets() {
		if preset.KB == speedKB {
			return i
		}
	}
	return -1
}

// trayMenuModel is the state the tray menu is painted from.
//
// It is plain data so the enable/check rules can be tested without a tray icon;
// systray itself cannot be driven from a unit test.
type trayMenuModel struct {
	// CanPause mirrors Manager.StopAll: there is something to pause.
	CanPause bool
	// CanResume mirrors Manager.StartAll: there is something to resume.
	CanResume bool
	// SpeedKB is the configured total cap, 0 for unlimited.
	SpeedKB int
}

// buildTrayMenuModel derives the menu state from the task counters and settings.
//
// It takes the same counters the sidebar shows (active = transferring, plus the
// queued/paused/failed/canceled buckets), so the tray and the main window can
// never disagree about whether anything is running.
func buildTrayMenuModel(stats map[string]int64, speedLimitKB int) trayMenuModel {
	return trayMenuModel{
		CanPause:  stats["active"] > 0 || stats["queued"] > 0,
		CanResume: stats["paused"] > 0 || stats["failed"] > 0 || stats["canceled"] > 0 || stats["queued"] > 0,
		SpeedKB:   speedLimitKB,
	}
}

// traySpeedItem pairs a preset with the live menu item that selects it.
type traySpeedItem struct {
	preset traySpeedPreset
	item   *systray.MenuItem
}

func (a *App) onTrayReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("VKMDownload")
	systray.SetTooltip("VKMDownload")

	show := systray.AddMenuItem("显示窗口", "显示主窗口")
	systray.AddSeparator()
	pauseAll := systray.AddMenuItem("暂停全部", "暂停所有正在下载和排队的任务")
	resumeAll := systray.AddMenuItem("继续全部", "继续所有已暂停、失败或取消的任务")
	speed := systray.AddMenuItem("限速", "限制总下载速度")
	speedItems := make([]traySpeedItem, 0, len(traySpeedPresets()))
	for _, preset := range traySpeedPresets() {
		tooltip := "不限制总下载速度"
		if preset.KB > 0 {
			tooltip = "把总下载速度限制为 " + preset.Label
		}
		speedItems = append(speedItems, traySpeedItem{
			preset: preset,
			item:   speed.AddSubMenuItemCheckbox(preset.Label, tooltip, false),
		})
	}
	systray.AddSeparator()
	quit := systray.AddMenuItem("退出", "退出程序")

	// paint applies the model to the real items. It is called once at start,
	// after every action, and on the refresh ticker. Only changes are pushed to
	// systray (a no-op repaint every two seconds would churn the native menu for
	// nothing), and a mutex serialises the painters — several goroutines call it.
	var (
		paintMu    sync.Mutex
		paintFirst = true
		lastPause  bool
		lastResume bool
		lastSpeed  = -2 // an index no preset can have: force the first paint
	)
	paint := func() {
		model := buildTrayMenuModel(a.mgr.Stats(), a.Config().SpeedLimitKB)
		checked := trayCheckedSpeedIndex(model.SpeedKB)

		paintMu.Lock()
		defer paintMu.Unlock()
		if paintFirst || model.CanPause != lastPause {
			if model.CanPause {
				pauseAll.Enable()
			} else {
				pauseAll.Disable()
			}
			lastPause = model.CanPause
		}
		if paintFirst || model.CanResume != lastResume {
			if model.CanResume {
				resumeAll.Enable()
			} else {
				resumeAll.Disable()
			}
			lastResume = model.CanResume
		}
		if checked != lastSpeed {
			for i, si := range speedItems {
				if i == checked {
					si.item.Check()
				} else {
					si.item.Uncheck()
				}
			}
			lastSpeed = checked
		}
		paintFirst = false
	}
	paint()

	go func() {
		for range show.ClickedCh {
			if a.ctx != nil {
				runtime.WindowShow(a.ctx)
			}
		}
	}()
	go func() {
		for range pauseAll.ClickedCh {
			// The action runs off the click loop: pausing every task can touch
			// the engine and the task file, and the menu must stay responsive
			// for the next click.
			go func() {
				a.debugLog("tray: paused %d task(s)", a.StopAllTasks())
				paint()
			}()
		}
	}()
	go func() {
		for range resumeAll.ClickedCh {
			go func() {
				a.debugLog("tray: resumed %d task(s)", a.StartAllTasks())
				paint()
			}()
		}
	}()
	for _, si := range speedItems {
		si := si
		go func() {
			for range si.item.ClickedCh {
				// SetConfig persists config.json and retunes the shared limiter;
				// it never runs on the menu's message loop.
				go func() {
					a.setSpeedLimitFromTray(si.preset.KB)
					paint()
				}()
			}
		}()
	}
	go func() {
		for range quit.ClickedCh {
			// Explicit exit: mark intent BEFORE asking Wails to quit, or
			// Frontend.Quit re-enters beforeClose (which hides when
			// MinimizeToTray is on) and vetoes the quit, leaving a
			// headless process. shutdown/stopTray removes the icon.
			a.requestQuit()
			if a.ctx != nil {
				runtime.Quit(a.ctx)
			} else {
				systray.Quit()
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(trayStateRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				paint()
			case <-trayExitCh:
				return
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

// setSpeedLimitFromTray writes the 限速 choice into the settings.
//
// SetConfig is the same path the settings page uses: it persists the file, keeps
// the live limiter in step and never restarts a transfer. An unchanged value is
// a no-op, so clicking the checked preset does not rewrite config.json.
func (a *App) setSpeedLimitFromTray(kb int) {
	cfg := a.Config()
	if cfg.SpeedLimitKB == kb {
		return
	}
	cfg.SpeedLimitKB = kb
	if err := a.SetConfig(cfg); err != nil {
		a.debugLog("tray: cannot set the speed limit: %v", err)
	}
}

func (a *App) onTrayExit() {
	// Stop the repaint goroutine; without this it would tick against a dead
	// menu for the rest of the process.
	trayExitOnce.Do(func() { close(trayExitCh) })
}

// stopTray removes the icon. Called on shutdown; without it a ghost icon can
// linger until the user hovers it.
func (a *App) stopTray() {
	systray.Quit()
}
