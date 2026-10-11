package main

import (
	"context"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the object exposed to the front-end. Every exported method becomes a
// callable binding, so internal state is kept unexported.
type App struct {
	ctx context.Context
}

// NewApp creates the application object.
func NewApp() *App {
	return &App{}
}

// startup is called once the runtime is ready but before the window is shown.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// domReady runs after the front-end has finished loading. Work that must not
// race the first render belongs here rather than in startup.
func (a *App) domReady(ctx context.Context) {
	_ = ctx
}

// shutdown releases resources as the application exits.
func (a *App) shutdown(ctx context.Context) {
	_ = ctx
}

// beforeClose is consulted when the user closes the window; returning true lets
// the close proceed.
//
// This will later refuse the close and hide the window instead while downloads
// are still running.
func (a *App) beforeClose(ctx context.Context) bool {
	_ = ctx
	return true
}

// Version reports the build identifier shown in the UI.
func (a *App) Version() string {
	return "0.1.0-dev"
}

// Quit terminates the application, backing the front-end's own exit control.
func (a *App) Quit() {
	runtime.Quit(a.ctx)
}
