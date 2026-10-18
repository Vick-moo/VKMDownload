package main

// Removing credentials that earlier versions left on disk.
//
// Two files accumulated data that no longer serves any purpose and is pure
// exposure once the download is over:
//
//   - tasks.json kept every download's full URL. A signed link carries its own
//     credential in the query string (token=, sig=, jwt=, X-Amz-Signature=,
//     Azure's se/skt/skoid…), so the history file became a list of working
//     credentials for sites the user had ever downloaded from. Measured on this
//     machine: 69 of 329 stored tasks had a query string, including Azure SAS
//     signatures and Google Drive export tokens.
//   - vkmdownload.log kept the same URLs, and the log is exactly the file a user
//     is asked to attach to a bug report. Measured: 173 lines with a signed URL.
//
// New writes are already safe (finished tasks are redacted before they are
// persisted, and debugLog sanitises every line). This is the one-time cleanup
// for what is already there, run at startup so it also happens for a user who
// never triggers another save.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// scrubStoredSecrets cleans the two files described above. It is best-effort and
// never fails startup: a file it cannot rewrite simply keeps its old content,
// and the next launch tries again.
func (a *App) scrubStoredSecrets() {
	a.scrubTaskHistory()
	a.sealStoredCookies()
	a.scrubTaskBackups()
	a.scrubLogs()
}

// sealStoredCookies encrypts cookies that an older build wrote in plaintext.
//
// Without this the change would only protect future saves: a record that is
// never written again (a task paused months ago) would keep its plaintext cookie
// forever, which is exactly the case the change exists for.
func (a *App) sealStoredCookies() {
	path := filepath.Join(a.dataDir(), "tasks.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var list []persistedTask
	if err := json.Unmarshal(b, &list); err != nil {
		return
	}

	sealed := 0
	for i := range list {
		p := &list[i]
		if p.Cookie == "" || hasSecretPrefix(p.Cookie) || finishedForHistory(p.Status) {
			continue
		}
		if enc, serr := sealCookie(p.Cookie); serr == nil {
			p.Cookie = enc
			sealed++
		} else {
			// Better to lose the cookie than to keep it in the clear.
			a.debugLog("privacy: cannot encrypt a stored cookie, dropping it: %v", serr)
			p.Cookie = ""
		}
	}
	if sealed == 0 {
		return
	}
	out, merr := json.MarshalIndent(list, "", "  ")
	if merr != nil {
		return
	}
	if werr := writeFileAtomic(path, out); werr != nil {
		a.debugLog("privacy: cannot rewrite tasks.json with encrypted cookies: %v", werr)
		return
	}
	a.debugLog("privacy: encrypted %d stored cookies", sealed)
}

// scrubTaskBackups cleans the copies tasks.json leaves behind: the corrupt file
// loadTasks renames aside, and the temporary file a crash can strand.
//
// They hold the same records as the index — which is to say the same signed URLs
// and cookies — and nothing ever reads them again, so leaving them would make
// "the credentials were removed" true only of the file the app opens.
func (a *App) scrubTaskBackups() {
	dir := a.dataDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		isBackup := strings.HasPrefix(name, "tasks.json.corrupt-") ||
			strings.HasPrefix(name, "tasks.backup-") ||
			(strings.HasPrefix(name, "tasks.json.") && strings.HasSuffix(name, ".tmp"))
		if !isBackup {
			continue
		}
		path := filepath.Join(dir, name)
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			continue
		}
		// A cookie value has no recognisable shape, so the whole copy goes: a
		// crash copy of the index has no diagnostic value that outweighs a
		// plaintext browser session.
		if strings.Contains(string(b), `"cookie"`) {
			if rerr := os.Remove(path); rerr != nil {
				a.debugLog("privacy: cannot remove a stale task copy %s: %v", name, rerr)
			}
			continue
		}
		// Anything else keeps its shape; only URL query strings are stripped.
		// These files exist because something about them is already broken, so a
		// regex over their text is the one edit that cannot make them worse.
		if cleaned := sanitizeLogText(string(b)); cleaned != string(b) {
			_ = writeFileAtomic(path, []byte(cleaned))
		}
	}
}

// scrubTaskHistory rewrites tasks.json without credentials for finished tasks.
//
// It edits the file rather than the live table because it runs before loadTasks:
// the freshly loaded tasks are then already clean, so an in-memory copy never
// holds a credential it does not need.
func (a *App) scrubTaskHistory() {
	path := filepath.Join(a.dataDir(), "tasks.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var list []persistedTask
	if err := json.Unmarshal(b, &list); err != nil {
		// Corrupt: loadTasks renames it aside and reports; not this function's
		// business to decide what a broken index means.
		return
	}

	redacted, cleared := 0, 0
	for i := range list {
		p := &list[i]
		if !finishedForHistory(p.Status) {
			continue
		}
		if cleaned := redactedForHistory(p.URL); cleaned != p.URL {
			p.URL = cleaned
			redacted++
		}
		if p.Cookie != "" || p.Referer != "" || p.UserAgent != "" {
			p.Cookie, p.Referer, p.UserAgent = "", "", ""
			cleared++
		}
	}
	if redacted == 0 && cleared == 0 {
		return
	}

	out, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(path, out); err != nil {
		a.debugLog("privacy: cannot rewrite tasks.json: %v", err)
		return
	}
	a.debugLog("privacy: removed credentials from %d finished tasks (URLs redacted: %d)", cleared, redacted)
}

// scrubLogs rewrites the app log and its rotated copy with every URL reduced to
// host and path.
func (a *App) scrubLogs() {
	// The writer takes this lock for each line; holding it here keeps a log
	// line from being appended between the read and the write and lost.
	debugLogMu.Lock()
	defer debugLogMu.Unlock()

	for _, name := range []string{"vkmdownload.log", "vkmdownload.log.1"} {
		path := filepath.Join(a.dataDir(), name)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		original := string(b)
		cleaned := sanitizeLogText(original)
		if cleaned == original {
			continue
		}
		if err := writeFileAtomic(path, []byte(cleaned)); err != nil {
			// Nothing to report it to: the log is the report channel.
			continue
		}
	}
}

// ---------------------------------------------------------------------------
// what a "clear my data" action needs

// StoredSecretSummary describes what the app currently keeps on disk about the
// user's downloads. The settings page shows it so the user can decide, and the
// clear action below acts on the same numbers.
//
// It deliberately reports counts, never values: the point is "what is kept",
// not another copy of it.
type StoredSecretSummary struct {
	// TaskCount is how many task records exist.
	TaskCount int `json:"taskCount"`
	// WithCookie counts records that still hold a browser cookie. Only
	// unfinished downloads have one now: a finished download replayed no
	// credentials, so they are dropped when it settles.
	WithCookie int `json:"withCookie"`
	// WithQueryURL counts records whose URL still carries a query string —
	// that is, a possibly signed link that can still be used to resume.
	WithQueryURL int `json:"withQueryUrl"`
	// LogBytes is the size of the app log, which records what the app did.
	LogBytes int64 `json:"logBytes"`
}

// StoredSecrets reports what is on disk, without revealing any of it.
func (a *App) StoredSecrets() StoredSecretSummary {
	var out StoredSecretSummary

	if b, err := os.ReadFile(filepath.Join(a.dataDir(), "tasks.json")); err == nil {
		var list []persistedTask
		if json.Unmarshal(b, &list) == nil {
			out.TaskCount = len(list)
			for _, p := range list {
				if p.Cookie != "" {
					out.WithCookie++
				}
				if strings.Contains(p.URL, "?") {
					out.WithQueryURL++
				}
			}
		}
	}
	if st, err := os.Stat(filepath.Join(a.dataDir(), "vkmdownload.log")); err == nil {
		out.LogBytes = st.Size()
	}
	return out
}

// ClearDownloadHistory deletes the task records and the log.
//
// It does not touch the downloaded files: they are the user's data, and a
// privacy action that silently removed their downloads would be a disaster. It
// also leaves the settings and the pairing key alone — those are configuration,
// not history.
func (a *App) ClearDownloadHistory() error {
	m := a.mgr

	// Hold the index writer for the whole operation. Emptying the map and then
	// deleting the file is not enough on its own: a saveTasks that began before
	// the map was cleared can still be inside its write and would put the records
	// back after the delete. saveMu is the lock those writes take.
	m.saveMu.Lock()
	defer m.saveMu.Unlock()

	m.mu.Lock()
	m.tasks = make(map[string]*task)
	m.order = nil
	m.mu.Unlock()

	dir := a.dataDir()
	// Every file that can hold a task record or a log line, including the copies
	// earlier versions left behind.
	names := []string{"tasks.json", "tasks.backup-before-cleanup.json", "vkmdownload.log", "vkmdownload.log.1"}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			if strings.HasPrefix(n, "tasks.json.corrupt-") ||
				strings.HasPrefix(n, "tasks.backup-") ||
				(strings.HasPrefix(n, "tasks.json.") && strings.HasSuffix(n, ".tmp")) {
				names = append(names, n)
			}
		}
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 %s 失败: %w", name, err)
		}
	}
	// Deliberately not logged: writing a line here would recreate the very file
	// the user just asked to have removed.
	return nil
}

// ---------------------------------------------------------------------------
// retention

// historyRetention is how long a finished task record is kept before the oldest
// ones are dropped. Without it the history grows forever, which is both a
// privacy liability and the reason the file on this machine had 329 entries.
const historyRetention = 90 * 24 * time.Hour

// trimHistory drops finished records older than historyRetention. Unfinished
// tasks are never dropped: they are still work the user asked for.
func (a *App) trimHistory() {
	m := a.mgr
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()

	cutoff := time.Now().Add(-historyRetention).Format(time.RFC3339Nano)
	var stale []string
	for _, id := range ids {
		v, ok := m.view(id)
		if !ok {
			continue
		}
		if v.Status != StatusCompleted && v.Status != StatusCanceled && v.Status != StatusFailed {
			continue
		}
		// Created is RFC3339Nano, so a string compare orders correctly for the
		// same zone; a record with no timestamp is kept.
		if v.Created != "" && v.Created < cutoff {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		return
	}
	n := m.RemoveTasks(stale)
	a.debugLog("privacy: trimmed %d finished tasks older than %s", n, historyRetention)
}
