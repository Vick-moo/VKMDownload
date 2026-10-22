package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config holds every user setting. Each one is an independent switch so the
// behaviour it controls can be turned off without losing the others.
type Config struct {
	// AutoStart begins a task as soon as it is created. With it off, a new task
	// is added to the list and waits for an explicit start.
	AutoStart bool `json:"autoStart"`

	// AskLocation opens the new-task dialog for every non-manual arrival
	// (browser extension, clipboard) so the save location is confirmed before
	// anything is created. Closing the dialog abandons that download. With it
	// off, arrivals follow AutoStart / the clipboard switches instead.
	AskLocation bool `json:"askLocation"`

	// OrganizeEnabled sorts automatic arrivals into group folders when they
	// complete. Manual tasks (the dialog) and anything whose extension
	// matches no group stay where they were put. Folders appear lazily: a
	// group folder is created only when its first file finishes.
	//
	// The folders live under OrganizeDir, not DownloadDir: the two are
	// independent roots, so pointing downloads at a test folder does not drag
	// the groups along with it.
	OrganizeEnabled bool            `json:"organizeEnabled"`
	OrganizeGroups  []OrganizeGroup `json:"organizeGroups"`
	// OrganizeDir is the root for relative group folders. Empty means the
	// system Downloads folder, resolved per machine like DownloadDir.
	OrganizeDir string `json:"organizeDir"`

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
	// ShutdownDelaySeconds is the cancel window before that shutdown, in
	// seconds (0..600, default 60). 0 means shut down immediately, with no
	// window to run "shutdown /a". A negative value is a typo and is reset to
	// the default by normalize: an accidental instant power-off is the one
	// failure mode this setting must not have.
	ShutdownDelaySeconds int `json:"shutdownDelaySeconds"`

	// OpenFileWhenDone opens the finished file; OpenDirWhenDone reveals it in
	// Explorer. They are mutually exclusive: normalize keeps OpenDirWhenDone
	// and clears OpenFileWhenDone when both are set, matching the runtime
	// priority in afterTaskFinished (the folder wins and the file is not also
	// opened).
	OpenFileWhenDone bool `json:"openFileWhenDone"`
	OpenDirWhenDone  bool `json:"openDirWhenDone"`

	// Browser extension control.
	//
	// The switches live here rather than in the extension so there is one place
	// to change them: the extension asks the app for the current policy before
	// every interception, which means a change takes effect at once and needs no
	// reinstall.
	//
	// ExtensionEnabled is the master switch.
	ExtensionEnabled bool `json:"extensionEnabled"`
	// ExtensionMinSizeKB is the smallest download the extension will take over,
	// in KiB. Anything smaller is left to the browser.
	//
	// The reason this exists at all: an extension that intercepts everything
	// also intercepts browser auto-updates, tracking pixels and the images in a
	// PDF viewer, which is the fastest way to make someone uninstall it. Zero
	// means take everything.
	ExtensionMinSizeKB int `json:"extensionMinSizeKb"`
	// ExtensionSkipHosts lists hosts the extension leaves alone, one per line.
	// A user who has one site that must download normally can exclude just that.
	ExtensionSkipHosts string `json:"extensionSkipHosts"`

	// MinimizeToTray hides the window instead of exiting, so downloads continue.
	MinimizeToTray bool `json:"minimizeToTray"`

	// LaunchAtLogin registers the app in the current user's Run key so it
	// starts with Windows. On by default: a download manager that is not
	// running cannot take over anything. The key is synced once at startup
	// and only rewritten when the stored value differs.
	LaunchAtLogin bool `json:"launchAtLogin"`

	// CheckUpdatesOnStart asks GitHub for a newer release once per launch. It is
	// on by default — an update nobody is told about is not much of a feature —
	// but it is the only request this program makes that is not a download, so
	// the user has to be able to turn it off. The manual "check now" button in
	// settings always works regardless of this switch.
	//
	// A pointer would distinguish "unset" from false; instead the default is
	// written into the file on first run, which is simpler and visible to anyone
	// who reads config.json.
	CheckUpdatesOnStart bool `json:"checkUpdatesOnStart"`

	// HistoryRetentionDays is how long a finished task record is kept, in days
	// (0..3650, default 90). 0 means keep it forever. Only completed, canceled
	// and failed records are ever dropped, and only at startup; unfinished work
	// is never trimmed. A negative value is a typo and resets to the default —
	// "keep forever" must never be the result of an accident.
	HistoryRetentionDays int `json:"historyRetentionDays"`

	// LoggingEnabled writes the diagnostic log file (vkmdownload.log). On by
	// default. With it off, debugLog writes nothing at all — the file is not
	// even created — while errors that must surface go through the runtime's
	// own logger (stderr in development builds). The one-time credential
	// scrubbing of an existing log still runs, since that removes data rather
	// than adding it.
	LoggingEnabled bool `json:"loggingEnabled"`

	// ClipboardWatch polls the clipboard for URLs.
	ClipboardWatch bool `json:"clipboardWatch"`
	// ClipboardAutoStart downloads a captured URL immediately; with it off the
	// URL is only added to the list.
	ClipboardAutoStart bool `json:"clipboardAutoStart"`
	// ClipboardConfirm asks before acting on a captured URL.
	ClipboardConfirm bool `json:"clipboardConfirm"`
	// ClipboardIntervalMs is the polling period, clamped by normalize to
	// [200, 10000] ms. The floor is honoured rather than replaced by the
	// default: a smaller number means "poll faster", not "reset".
	ClipboardIntervalMs int `json:"clipboardIntervalMs"`

	// RefreshRate controls how often progress numbers update: fast (250ms),
	// medium (650ms) or slow (1000ms). It only changes the UI refresh
	// cadence, never the transfer speed. Applies to newly started runs.
	RefreshRate string `json:"refreshRate"`

	// ResumeFlushKB caps how much downloaded data may go unsaved: the sidecar
	// is flushed at least this often by volume. Larger means fewer disk
	// writes but more re-download after a crash. 0/negative falls back to
	// the default.
	ResumeFlushKB int `json:"resumeFlushKb"`
	// ResumeFlushMs caps how long downloaded data may go unsaved, same
	// trade-off on the time axis.
	ResumeFlushMs int `json:"resumeFlushMs"`
	// ResumeFlushPct adds a size-relative trigger: flush at least every N% of
	// the total. Unknown total disables it too. With ResumeFlushByPct on and
	// no value (0, or an empty input box), normalize supplies the 10% default;
	// 0 with the switch off stays off.
	ResumeFlushPct int `json:"resumeFlushPct"`
	// The three By* switches let the user run any subset of the triggers.
	// At least one must stay on (enforced in normalize).
	ResumeFlushBySize bool `json:"resumeFlushBySize"`
	ResumeFlushByTime bool `json:"resumeFlushByTime"`
	ResumeFlushByPct  bool `json:"resumeFlushByPct"`

	// VerifyPeerProcess, when true, makes the loopback endpoint confirm the
	// calling process is a genuine signed browser (see peercheck.go) before
	// honouring a request. On by default: it needs no setup, and it closes
	// the forged-Origin hole for everything except an attacker already
	// injected into the browser itself.
	VerifyPeerProcess bool `json:"verifyPeerProcess"`
}

// OrganizeGroup maps filename extensions (without the dot, any case) to one
// folder. Dir empty means a folder called Name under the organize root
// (portable across machines); a picked Dir is absolute and wins over Name.
type OrganizeGroup struct {
	Name string   `json:"name"`
	Exts []string `json:"exts"`
	Dir  string   `json:"dir"`
}

// DefaultConfig returns the settings a fresh install starts with.
//
// The defaults are deliberately conservative about doing things on the user's
// behalf: clipboard watching and auto-start are off, so nothing downloads
// without an explicit action until the user asks for it.
func DefaultConfig() Config {
	return Config{
		AutoStart:            true,
		AskLocation:          true,
		DownloadDir:          defaultDownloadDir(),
		Connections:          8,
		MinSegmentSize:       1 << 20,
		Proxy:                "",
		MaxConcurrent:        2,
		SpeedLimitKB:         0,
		PlanSound:            false,
		PlanShutdown:         false,
		ShutdownDelaySeconds: 60,
		OpenFileWhenDone:     false,
		OpenDirWhenDone:      false,
		MinimizeToTray:       true,
		LaunchAtLogin:        true,
		CheckUpdatesOnStart:  true,
		HistoryRetentionDays: 90,
		LoggingEnabled:       true,
		ClipboardWatch:       false,
		ClipboardAutoStart:   false,
		ClipboardConfirm:     true,
		ClipboardIntervalMs:  800,
		RefreshRate:          "fast",
		ResumeFlushKB:        1024,
		ResumeFlushMs:        250,
		ResumeFlushPct:       0,
		ResumeFlushBySize:    true,
		ResumeFlushByTime:    true,
		ResumeFlushByPct:     false,

		// On by default, but only for downloads big enough to be worth a
		// separate program. Taking over everything — including the browser's own
		// updates and every favicon a page requests — is the fastest way to make
		// someone uninstall the extension.
		ExtensionEnabled:   true,
		ExtensionMinSizeKB: 1024,
		ExtensionSkipHosts: "",
		VerifyPeerProcess:  true,

		// Off by default: sorting is a surprise until the user asks for it.
		OrganizeEnabled: false,
		OrganizeDir:     defaultDownloadDir(),
		OrganizeGroups: []OrganizeGroup{
			{Name: "Programs", Exts: []string{"exe", "msi", "bat", "cmd", "ps1", "com", "scr"}},
			{Name: "Documents", Exts: []string{"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "md", "csv"}},
			{Name: "Archives", Exts: []string{"zip", "rar", "7z", "tar", "gz", "bz2", "xz", "iso"}},
		},
	}
}

// DefaultOrganizeGroups returns a fresh copy of the default group table for
// the settings page's reset button.
func (a *App) DefaultOrganizeGroups() []OrganizeGroup {
	return DefaultConfig().OrganizeGroups
}
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
	if c.Connections > maxConns {
		c.Connections = maxConns
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
	if c.ExtensionMinSizeKB < 0 {
		c.ExtensionMinSizeKB = 0
	}
	// A gigabyte is the point past which the filter is really "never take
	// anything", which is what the master switch is for.
	if c.ExtensionMinSizeKB > 1024*1024 {
		c.ExtensionMinSizeKB = 1024 * 1024
	}
	// History retention: 0 is a valid "keep forever". Only a negative (a typo)
	// is corrected, and it resets to the default rather than to 0, so a stray
	// minus cannot silently turn a 90-day policy into forever.
	if c.HistoryRetentionDays < 0 {
		c.HistoryRetentionDays = d.HistoryRetentionDays
	}
	if c.HistoryRetentionDays > 3650 {
		c.HistoryRetentionDays = 3650
	}
	// Same rule for the shutdown window: 0 is an explicit "shut down now",
	// while a negative would mean "now" by accident.
	if c.ShutdownDelaySeconds < 0 {
		c.ShutdownDelaySeconds = d.ShutdownDelaySeconds
	}
	if c.ShutdownDelaySeconds > 600 {
		c.ShutdownDelaySeconds = 600
	}
	// Clamp to the floor rather than to the 800ms default: typing 100 to poll
	// faster used to make the app eight times slower, with no explanation. The
	// floor is the smallest period the poller is allowed to run at.
	if c.ClipboardIntervalMs < 200 {
		c.ClipboardIntervalMs = 200
	}
	if c.ClipboardIntervalMs > 10000 {
		c.ClipboardIntervalMs = 10000
	}
	c.OrganizeGroups = normalizeOrganizeGroups(c.OrganizeGroups)
	if strings.TrimSpace(c.OrganizeDir) == "" {
		c.OrganizeDir = d.OrganizeDir
	}
	switch strings.ToLower(strings.TrimSpace(c.RefreshRate)) {
	case "medium", "slow":
		c.RefreshRate = strings.ToLower(strings.TrimSpace(c.RefreshRate))
	default:
		// Unknown (including empty from older configs) means today's
		// behaviour: no visible change on upgrade.
		c.RefreshRate = "fast"
	}
	// Bounds for the resume-flush knobs: below this the sidecar churns for
	// no benefit, above this a crash costs real re-download minutes.
	if c.ResumeFlushKB < 64 {
		c.ResumeFlushKB = 1024
	}
	if c.ResumeFlushKB > 64<<10 {
		c.ResumeFlushKB = 64 << 10
	}
	if c.ResumeFlushMs < 100 {
		c.ResumeFlushMs = 250
	}
	if c.ResumeFlushMs > 10000 {
		c.ResumeFlushMs = 10000
	}
	if c.ResumeFlushPct < 0 {
		c.ResumeFlushPct = 0
	}
	if c.ResumeFlushPct > 100 {
		c.ResumeFlushPct = 100
	}
	// The switch and the value have to agree: the engine reads 0 as "off", so
	// ticking "按百分比保存" while the field was empty (or 0) left the trigger
	// silently disabled. An enabled trigger with no value takes the 10% default;
	// an explicit 0 with the box unticked still means off.
	if c.ResumeFlushByPct && c.ResumeFlushPct <= 0 {
		c.ResumeFlushPct = 10
	}
	// At least one save trigger must stay on: with everything off a crash
	// would silently discard the whole transfer. Force the time trigger
	// back on rather than inventing values for the others.
	if !c.ResumeFlushBySize && !c.ResumeFlushByTime && !c.ResumeFlushByPct {
		c.ResumeFlushByTime = true
		if c.ResumeFlushMs < 100 {
			c.ResumeFlushMs = 250
		}
	}
	// One completion action at a time. Both true would open the folder and the
	// file on top of each other; afterTaskFinished already gives the folder
	// priority, so normalize stores that same rule rather than leaving a pair
	// the settings UI would show as two selected options.
	if c.OpenFileWhenDone && c.OpenDirWhenDone {
		c.OpenFileWhenDone = false
	}
}

// normalizeOrganizeGroups cleans the group table: names are trimmed and must
// survive sanitizeSuggestedName (no separators, no device names) unless an
// absolute Dir carries the location instead, extensions are lower-cased
// without the dot, and empty entries go away. First group wins on overlap,
// so order is preserved.
//
// It always builds a new slice. The old version reused the caller's backing
// array (out := groups[:0]) and overwrote it entry by entry, so normalising a
// copy of the live settings wrote straight into the live settings — the race
// detector caught exactly that. The Exts slices are copied for the same reason.
func normalizeOrganizeGroups(groups []OrganizeGroup) []OrganizeGroup {
	out := make([]OrganizeGroup, 0, len(groups))
	seen := map[string]bool{}
	for _, g := range groups {
		name := sanitizeSuggestedName(g.Name)
		dir := strings.TrimSpace(g.Dir)
		if dir != "" {
			dir = filepath.Clean(dir)
		}
		if name == "" && dir == "" {
			continue
		}
		var exts []string
		for _, e := range g.Exts {
			e = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(e), ".")))
			if e == "" || e == "." || e == ".." || strings.ContainsAny(e, `/\`) || seen[name+"\x00"+e] {
				continue
			}
			seen[name+"\x00"+e] = true
			exts = append(exts, e)
		}
		if len(exts) == 0 {
			continue
		}
		out = append(out, OrganizeGroup{Name: name, Exts: exts, Dir: dir})
	}
	return out
}

// clone returns a copy that shares no memory with c. Config() hands out one and
// SetConfig stores one: every slice a caller can reach must be its own, or a
// settings dialog (or a test) editing one field mutates the live configuration
// from outside the lock.
func (c Config) clone() Config {
	out := c
	if c.OrganizeGroups != nil {
		out.OrganizeGroups = make([]OrganizeGroup, len(c.OrganizeGroups))
		for i, g := range c.OrganizeGroups {
			out.OrganizeGroups[i] = g
			if g.Exts != nil {
				out.OrganizeGroups[i].Exts = append([]string(nil), g.Exts...)
			}
		}
	}
	return out
}

// organizeTarget maps a finished filename to its group folder. A group with
// an absolute Dir lands there; otherwise it lands under root (the organize
// root, independent of the download directory). It returns "" when the name
// matches no group, in which case the file stays put.
func organizeTarget(groups []OrganizeGroup, root, name string) string {
	dot := strings.LastIndex(name, ".")
	if dot < 0 || dot == len(name)-1 {
		return ""
	}
	ext := strings.ToLower(name[dot+1:])
	for _, g := range groups {
		for _, e := range g.Exts {
			if e == ext {
				base := g.Dir
				if base == "" {
					base = filepath.Join(root, g.Name)
				}
				return filepath.Join(base, name)
			}
		}
	}
	return ""
}

// loadConfig reads the settings file, falling back to defaults. A corrupt file
// is kept aside rather than silently dropped: a replaced settings file is
// indistinguishable from a settings bug, so the bad copy is renamed with a
// timestamp (the same treatment tasks.json gets) and the event is logged.
// Losing preferences is a nuisance; refusing to start, or discarding them with
// no trace, is worse.
func loadConfig(path string, logf func(string, ...any)) Config {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		kept := fmt.Sprintf("%s.corrupt-%s", path, time.Now().Format("20060102-150405.000"))
		if rerr := os.Rename(path, kept); rerr != nil {
			kept = "(另存失败: " + rerr.Error() + ")"
		}
		if logf != nil {
			logf("config.json 损坏（%v），已另存为 %s，本次使用默认设置", err, kept)
		}
		return DefaultConfig()
	}
	cfg.normalize()
	return cfg
}

// saveConfig writes the settings atomically, so a crash mid-write cannot leave
// a half-written file that fails to parse on the next start.
func saveConfig(path string, cfg Config) error {
	// Normalised on a copy: normalize rewrites OrganizeGroups, and the caller
	// may still hold the value it passed in.
	cfg = cfg.clone()
	cfg.normalize()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	// Flushed before the rename, as tasks.json does: a crash must not leave a
	// correctly named but incomplete settings file behind.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
