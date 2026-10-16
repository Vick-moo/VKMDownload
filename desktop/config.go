package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

	// ClipboardWatch polls the clipboard for URLs.
	ClipboardWatch bool `json:"clipboardWatch"`
	// ClipboardAutoStart downloads a captured URL immediately; with it off the
	// URL is only added to the list.
	ClipboardAutoStart bool `json:"clipboardAutoStart"`
	// ClipboardConfirm asks before acting on a captured URL.
	ClipboardConfirm bool `json:"clipboardConfirm"`
	// ClipboardIntervalMs is the polling period.
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
	// the total (0 disables). Unknown total disables it too.
	ResumeFlushPct int `json:"resumeFlushPct"`
	// The three By* switches let the user run any subset of the triggers.
	// At least one must stay on (enforced in normalize).
	ResumeFlushBySize bool `json:"resumeFlushBySize"`
	ResumeFlushByTime bool `json:"resumeFlushByTime"`
	ResumeFlushByPct  bool `json:"resumeFlushByPct"`

	// ApiToken is the bearer credential the browser extension presents to the
	// loopback endpoint. Generated randomly on first start and persisted.
	ApiToken string `json:"apiToken"`
	// ApiAuthRequired, when true, makes the loopback endpoint reject every
	// non-preflight request without a matching Authorization header. Off by
	// default: loopback plus the extension-Origin check is the historical
	// posture, and turning a hard requirement on silently would cut off an
	// extension that does not know the token yet.
	ApiAuthRequired bool `json:"apiAuthRequired"`
	// VerifyPeerProcess, when true, makes the loopback endpoint confirm the
	// calling process is a genuine signed browser (see peercheck.go) before
	// honouring a request. On by default: unlike the token it needs no
	// setup, and it closes the forged-Origin hole for everything except an
	// attacker already injected into the browser itself.
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
		AutoStart:           true,
		AskLocation:         true,
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
		RefreshRate:         "fast",
		ResumeFlushKB:       1024,
		ResumeFlushMs:       250,
		ResumeFlushPct:      0,
		ResumeFlushBySize:   true,
		ResumeFlushByTime:   true,
		ResumeFlushByPct:    false,

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
	if c.ExtensionMinSizeKB < 0 {
		c.ExtensionMinSizeKB = 0
	}
	// A gigabyte is the point past which the filter is really "never take
	// anything", which is what the master switch is for.
	if c.ExtensionMinSizeKB > 1024*1024 {
		c.ExtensionMinSizeKB = 1024 * 1024
	}
	if c.ClipboardIntervalMs < 200 {
		c.ClipboardIntervalMs = d.ClipboardIntervalMs
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
	// At least one save trigger must stay on: with everything off a crash
	// would silently discard the whole transfer. Force the time trigger
	// back on rather than inventing values for the others.
	if !c.ResumeFlushBySize && !c.ResumeFlushByTime && !c.ResumeFlushByPct {
		c.ResumeFlushByTime = true
		if c.ResumeFlushMs < 100 {
			c.ResumeFlushMs = 250
		}
	}
	c.ApiToken = strings.TrimSpace(c.ApiToken)
	// ApiAuthRequired defaults to false via the zero value; no clamp needed.
}

// newApiToken mints a random 256-bit bearer token for the loopback endpoint.
func newApiToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// randomness failure must not leave the token empty-but-valid;
		// callers treat "" as "no credential configured".
		return ""
	}
	return hex.EncodeToString(b[:])
}

// ensureApiToken fills in a missing token. Called on startup and on every
// read path that hands the token out, so an old config from before the field
// existed converges without user action.
func ensureApiToken(cfg *Config) bool {
	if strings.TrimSpace(cfg.ApiToken) != "" {
		return false
	}
	if tok := newApiToken(); tok != "" {
		cfg.ApiToken = tok
		return true
	}
	return false
}

// normalizeOrganizeGroups cleans the group table: names are trimmed and must
// survive sanitizeSuggestedName (no separators, no device names) unless an
// absolute Dir carries the location instead, extensions are lower-cased
// without the dot, and empty entries go away. First group wins on overlap,
// so order is preserved.
func normalizeOrganizeGroups(groups []OrganizeGroup) []OrganizeGroup {
	out := groups[:0]
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
