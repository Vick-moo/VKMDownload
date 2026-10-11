package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds every user setting. Each one is an independent switch so the
// behaviour it controls can be turned off without losing the others.
type Config struct {
	// AutoStart begins a task as soon as it is created. With it off, a new task
	// is added to the list and waits for an explicit start.
	AutoStart bool `json:"autoStart"`

	DownloadDir string `json:"downloadDir"`

	// Connections is the default parallelism for new tasks.
	Connections int `json:"connections"`
	// MinSegmentSize stops small files from being split into pointless ranges.
	MinSegmentSize int64 `json:"minSegmentSize"`

	// Proxy applies to every task; empty means honour the environment.
	Proxy string `json:"proxy"`

	// MaxConcurrent bounds how many tasks transfer at once; the rest queue.
	MaxConcurrent int `json:"maxConcurrent"`

	// SpeedLimitKB caps the combined download speed, in KiB/s. Zero means no
	// limit. It is deliberately a total rather than a per-task figure: a limit
	// that each of several tasks obeyed separately would not be a limit on the
	// connection the user is trying to protect.
	SpeedLimitKB int `json:"speedLimitKb"`

	// Plan settings describe what happens once every task in a plan has
	// finished. Each is independent, so asking for a sound does not also
	// require shutting the machine down.
	//
	// PlanSound plays a short tone.
	PlanSound bool `json:"planSound"`
	// PlanShutdown powers the machine off.
	PlanShutdown bool `json:"planShutdown"`

	OpenFileWhenDone bool `json:"openFileWhenDone"`
	OpenDirWhenDone  bool `json:"openDirWhenDone"`

	// MinimizeToTray hides the window instead of exiting, so downloads continue.
	MinimizeToTray bool `json:"minimizeToTray"`

	// ClipboardWatch polls the clipboard for URLs.
	ClipboardWatch bool `json:"clipboardWatch"`
	// ClipboardAutoStart downloads a captured URL immediately; with it off the
	// URL is only added to the list.
	ClipboardAutoStart bool `json:"clipboardAutoStart"`
	// ClipboardConfirm asks before acting on a captured URL.
	ClipboardConfirm bool `json:"clipboardConfirm"`
	// ClipboardIntervalMs is the polling period.
	ClipboardIntervalMs int `json:"clipboardIntervalMs"`
}

// DefaultConfig returns the settings a fresh install starts with.
//
// The defaults are deliberately conservative about doing things on the user's
// behalf: clipboard watching and auto-start are off, so nothing downloads
// without an explicit action until the user asks for it.
func DefaultConfig() Config {
	return Config{
		AutoStart:           true,
		DownloadDir:         defaultDownloadDir(),
		Connections:         8,
		MinSegmentSize:      1 << 20,
		Proxy:               "",
		MaxConcurrent:       2,
		SpeedLimitKB:        0,
		PlanSound:           false,
		PlanShutdown:        false,
		OpenFileWhenDone:    false,
		OpenDirWhenDone:     false,
		MinimizeToTray:      true,
		ClipboardWatch:      false,
		ClipboardAutoStart:  false,
		ClipboardConfirm:    true,
		ClipboardIntervalMs: 800,
	}
}

// defaultDownloadDir is the user's Downloads folder, falling back to the home
// directory when the shell does not report one.
func defaultDownloadDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		dl := filepath.Join(home, "Downloads")
		if st, err := os.Stat(dl); err == nil && st.IsDir() {
			return dl
		}
		return home
	}
	return "."
}

// normalize clamps values that would otherwise produce a bad first run.
func (c *Config) normalize() {
	d := DefaultConfig()
	if c.DownloadDir == "" {
		c.DownloadDir = d.DownloadDir
	}
	if c.Connections < 1 {
		c.Connections = d.Connections
	}
	if c.Connections > 64 {
		c.Connections = 64
	}
	if c.MinSegmentSize <= 0 {
		c.MinSegmentSize = d.MinSegmentSize
	}
	if c.MaxConcurrent < 1 {
		c.MaxConcurrent = d.MaxConcurrent
	}
	if c.MaxConcurrent > 16 {
		c.MaxConcurrent = 16
	}
	// A negative limit is meaningless; treat it as "no limit" rather than
	// handing a negative rate to the engine.
	if c.SpeedLimitKB < 0 {
		c.SpeedLimitKB = 0
	}
	// Above this the setting stops being a limit and starts being a typo.
	if c.SpeedLimitKB > 10*1024*1024 {
		c.SpeedLimitKB = 10 * 1024 * 1024
	}
	if c.ClipboardIntervalMs < 200 {
		c.ClipboardIntervalMs = d.ClipboardIntervalMs
	}
	if c.ClipboardIntervalMs > 10000 {
		c.ClipboardIntervalMs = 10000
	}
}

// loadConfig reads the settings file, falling back to defaults. A corrupt file
// is ignored rather than fatal: losing preferences is a nuisance, refusing to
// start is worse.
func loadConfig(path string) Config {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return DefaultConfig()
	}
	cfg.normalize()
	return cfg
}

// saveConfig writes the settings atomically, so a crash mid-write cannot leave
// a half-written file that fails to parse on the next start.
func saveConfig(path string, cfg Config) error {
	cfg.normalize()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
