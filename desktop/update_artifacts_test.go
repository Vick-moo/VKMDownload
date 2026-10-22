package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ------------------------------------------------------- verification fields

// TestUpdateStateCarriesVerificationAndBackup pins the marker's new fields: they
// are written when the update is handed over and read back by the next launch,
// which is what lets the settings page show a result instead of a promise.
func TestUpdateStateCarriesVerificationAndBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-state.json")
	want := updateState{
		Stage:   stageConfirmed,
		Version: "0.2.9",
		Exe:     `C:\app\VKMDownload.exe`,
		Bak:     `C:\app\VKMDownload.exe.bak`,
		// Verification: digest was present, the length matched, and the file
		// passed every check.
		DigestPresent: true,
		SizeMatched:   true,
		Verified:      true,
		// The restore point.
		BackupAvailable: true,
		BackupVersion:   "0.2.8",
		BackupSize:      12345678,
	}
	if err := updateStateSave(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := updateStateLoad(path)
	if err != nil || got == nil {
		t.Fatalf("load: %v (state %v)", err, got)
	}
	if !got.DigestPresent || !got.SizeMatched || !got.Verified {
		t.Errorf("verification = digest:%v size:%v verified:%v, want all true",
			got.DigestPresent, got.SizeMatched, got.Verified)
	}
	if !got.BackupAvailable || got.BackupVersion != want.BackupVersion || got.BackupSize != want.BackupSize {
		t.Errorf("backup = %v %q %d, want true %q %d",
			got.BackupAvailable, got.BackupVersion, got.BackupSize,
			want.BackupVersion, want.BackupSize)
	}

	// The JSON names are the wire format the front-end reads; renaming one here
	// would silently make an older marker unreadable.
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, key := range []string{
		"digestPresent", "sizeMatched", "verified",
		"backupAvailable", "backupVersion", "backupSize",
	} {
		if !strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("marker is missing the %q field: %s", key, raw)
		}
	}
}

// TestDownloadUpdateRecordsVerification covers the three verdicts the UI can now
// show: digest present and matching, digest present and wrong, and no digest at
// all (where the length and the MZ header have to carry the check).
func TestDownloadUpdateRecordsVerification(t *testing.T) {
	payload := make([]byte, (1<<20)+64)
	copy(payload, "MZ")
	for i := 2; i < len(payload); i++ {
		payload[i] = byte('a' + i%26)
	}
	sum := sha256.Sum256(payload)
	goodDigest := "sha256:" + hex.EncodeToString(sum[:])

	cases := []struct {
		name          string
		digest        string
		wantState     string
		wantDigest    bool
		wantSizeMatch bool
		wantVerified  bool
	}{
		{"digest and length match", goodDigest, "ready", true, true, true},
		{"digest mismatch fails verification", "sha256:" + strings.Repeat("0", 64), "failed", true, true, false},
		{"no digest falls back to length and header", "", "ready", false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				_, _ = w.Write(payload)
			}))
			defer srv.Close()

			app := NewAppWithDataDir(dir)
			app.emit = func(string, ...any) {}
			staged := filepath.Join(dir, "VKMDownload.exe.new")

			app.downloadUpdate(releaseAsset{tag: "v9.9.9", exeURL: srv.URL, digest: c.digest}, staged)

			st := app.UpdateProgress()
			if st.State != c.wantState {
				t.Fatalf("state = %q, want %q (error %q)", st.State, c.wantState, st.Error)
			}
			if st.DigestPresent != c.wantDigest {
				t.Errorf("DigestPresent = %v, want %v", st.DigestPresent, c.wantDigest)
			}
			if st.SizeMatched != c.wantSizeMatch {
				t.Errorf("SizeMatched = %v, want %v", st.SizeMatched, c.wantSizeMatch)
			}
			if st.Verified != c.wantVerified {
				t.Errorf("Verified = %v, want %v", st.Verified, c.wantVerified)
			}
		})
	}
}

// TestHydrateUpdateStatusReadsTheLastAttempt covers the restart: the polled
// status carries the recorded verification and restore point, while State stays
// empty because this process has not started an update.
func TestHydrateUpdateStatusReadsTheLastAttempt(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "VKMDownload.exe")
	bak := exe + ".bak"
	writeFile(t, exe, oldExeContent)
	writeFile(t, bak, "OLD-BUILD")

	app := NewAppWithDataDir(dir)
	app.emit = func(string, ...any) {}
	app.sys = newSysStub(dir)
	if err := updateStateSave(app.updateStatePath(), updateState{
		Stage:           stageConfirmed,
		Exe:             exe,
		Bak:             bak,
		NewSHA:          "stale-hash",
		DigestPresent:   true,
		SizeMatched:     true,
		Verified:        false,
		BackupAvailable: true,
		BackupVersion:   "0.2.8",
		BackupSize:      42,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	app.hydrateUpdateStatus()

	st := app.UpdateProgress()
	if st.State != "" {
		t.Errorf("State = %q, want empty: a restart is not an update in progress", st.State)
	}
	if !st.DigestPresent || !st.SizeMatched || st.Verified {
		t.Errorf("verification = digest:%v size:%v verified:%v, want true/true/false",
			st.DigestPresent, st.SizeMatched, st.Verified)
	}
	if !st.BackupAvailable || st.BackupVersion != "0.2.8" || st.BackupSize != 42 {
		t.Errorf("backup = %v %q %d, want true 0.2.8 42",
			st.BackupAvailable, st.BackupVersion, st.BackupSize)
	}
}

// --------------------------------------------------------------- backup API

func TestUpdateBackupInfoReportsTheRestorePoint(t *testing.T) {
	dir := t.TempDir()
	app := NewAppWithDataDir(dir)
	app.emit = func(string, ...any) {}
	app.sys = newSysStub(dir)

	if got := app.UpdateBackupInfo(); got.Available {
		t.Fatalf("no record yet, but the backup reports %+v", got)
	}

	exe := filepath.Join(dir, "VKMDownload.exe")
	bak := exe + ".bak"
	writeFile(t, bak, "OLD-BUILD")
	if err := updateStateSave(app.updateStatePath(), updateState{
		Stage: stageConfirmed, Exe: exe, Bak: bak,
		BackupAvailable: true, BackupVersion: "0.2.8", BackupSize: 9,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := app.UpdateBackupInfo()
	if !got.Available {
		t.Fatal("the backup exists but is not reported as available")
	}
	if got.Version != "0.2.8" || got.Size != 9 || !samePathText(got.Path, bak) {
		t.Errorf("backup = %+v, want version 0.2.8 size 9 path %s", got, bak)
	}

	// A record can outlive its file. Offering a rollback that cannot work is
	// worse than saying there is nothing to restore.
	if err := os.Remove(bak); err != nil {
		t.Fatal(err)
	}
	if got := app.UpdateBackupInfo(); got.Available {
		t.Errorf("backup reported as available after its file was deleted: %+v", got)
	}
}

func TestDeleteUpdateBackup(t *testing.T) {
	t.Run("no backup is a clear error", func(t *testing.T) {
		app := NewAppWithDataDir(t.TempDir())
		app.emit = func(string, ...any) {}
		app.sys = newSysStub(t.TempDir())

		err := app.DeleteUpdateBackup()
		if err == nil {
			t.Fatal("deleting a backup that does not exist reported success")
		}
		if !strings.Contains(err.Error(), "没有可删除") {
			t.Errorf("error = %q, want a clear 'nothing to delete' message", err)
		}
	})

	t.Run("removes the file and clears the record", func(t *testing.T) {
		dir := t.TempDir()
		app := NewAppWithDataDir(dir)
		app.emit = func(string, ...any) {}
		app.sys = newSysStub(dir)

		exe := filepath.Join(dir, "VKMDownload.exe")
		bak := exe + ".bak"
		writeFile(t, bak, "OLD-BUILD")
		if err := updateStateSave(app.updateStatePath(), updateState{
			Stage: stageConfirmed, Exe: exe, Bak: bak,
			BackupAvailable: true, BackupVersion: "0.2.8", BackupSize: 9,
		}); err != nil {
			t.Fatalf("save: %v", err)
		}

		if err := app.DeleteUpdateBackup(); err != nil {
			t.Fatalf("DeleteUpdateBackup: %v", err)
		}
		if _, err := os.Stat(bak); !os.IsNotExist(err) {
			t.Errorf("the backup file is still there: %v", err)
		}

		st, err := updateStateLoad(app.updateStatePath())
		if err != nil || st == nil {
			t.Fatalf("the update record was dropped (load %v, state %v)", err, st)
		}
		if st.Bak != "" || st.BackupAvailable || st.BackupVersion != "" || st.BackupSize != 0 {
			t.Errorf("record still names a backup: bak=%q available=%v version=%q size=%d",
				st.Bak, st.BackupAvailable, st.BackupVersion, st.BackupSize)
		}

		// A second press has nothing left to remove and says so.
		if err := app.DeleteUpdateBackup(); err == nil {
			t.Error("a second delete reported success")
		}
	})

	t.Run("a refused delete keeps the record", func(t *testing.T) {
		dir := t.TempDir()
		app := NewAppWithDataDir(dir)
		app.emit = func(string, ...any) {}
		app.sys = newSysStub(dir)

		exe := filepath.Join(dir, "VKMDownload.exe")
		bak := exe + ".bak"
		// A non-empty directory in place of the file makes os.Remove fail on
		// every platform, so the error path is exercised without permissions.
		if err := os.MkdirAll(filepath.Join(bak, "child"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := updateStateSave(app.updateStatePath(), updateState{
			Stage: stageConfirmed, Exe: exe, Bak: bak, BackupAvailable: true,
		}); err != nil {
			t.Fatalf("save: %v", err)
		}

		if err := app.DeleteUpdateBackup(); err == nil {
			t.Fatal("deleting an undeletable backup reported success")
		}
		st, err := updateStateLoad(app.updateStatePath())
		if err != nil || st == nil || st.Bak != bak {
			t.Errorf("the record no longer names the backup after a failed delete: %+v (%v)", st, err)
		}
	})
}

// ------------------------------------------------------------ cleanup sweep

// artifactFixture builds an app whose executable lives in a temp directory, so
// the cleanup can be pointed at real files. It returns the app and the exe path.
func artifactFixture(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "VKMDownload.exe")
	writeFile(t, exe, "NEW-BUILD")
	app := NewAppWithDataDir(dir)
	app.emit = func(string, ...any) {}
	app.sys = newSysStub(dir)
	app.exePaths = func() (string, string, error) { return exe, exe + ".new", nil }
	return app, exe
}

// TestCleanupKeepsExactlyOneBackup covers "only one .bak": the record's backup
// survives even when it is the oldest, and every other copy goes. That ordering
// is deliberate — the record's copy is the restore point, so it can never be the
// one the sweep picks to delete.
func TestCleanupKeepsExactlyOneBackup(t *testing.T) {
	app, exe := artifactFixture(t)
	newSHA, err := hashFileSHA256(exe)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, exe+".bak", "RECORD-BACKUP")
	writeFile(t, exe+".bak.1", "LEGACY-OLD")
	writeFile(t, exe+".bak-2", "LEGACY-OTHER")
	// Make the record's copy the oldest so a "keep the newest" implementation
	// would fail this test.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(exe+".bak", old, old); err != nil {
		t.Fatal(err)
	}
	if err := updateStateSave(app.updateStatePath(), updateState{
		Stage: stageConfirmed, Exe: exe, NewSHA: newSHA, Bak: exe + ".bak",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	app.cleanupUpdateArtifacts()

	if _, err := os.Stat(exe + ".bak"); err != nil {
		t.Errorf("the record's backup was deleted: %v", err)
	}
	for _, p := range []string{exe + ".bak.1", exe + ".bak-2"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("the older backup %s survived (stat err %v)", p, err)
		}
	}
}

// TestCleanupKeepsBackupsWhenTheRecordIsUnreadable pins the safety rule: a
// marker that cannot be parsed says nothing about which copy is the only working
// one, so no backup is touched.
func TestCleanupKeepsBackupsWhenTheRecordIsUnreadable(t *testing.T) {
	app, exe := artifactFixture(t)
	writeFile(t, exe+".bak", "RECORD-BACKUP")
	writeFile(t, exe+".bak.1", "LEGACY-OLD")
	if err := os.WriteFile(app.updateStatePath(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	app.cleanupUpdateArtifacts()

	for _, p := range []string{exe + ".bak", exe + ".bak.1"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("backup %s was deleted on an unreadable record: %v", p, err)
		}
	}
}

// TestCleanupDropsFailedCopiesOlderThanADay covers the startup age rule: the
// copies are rollback evidence, so they are kept for a day and then swept.
func TestCleanupDropsFailedCopiesOlderThanADay(t *testing.T) {
	app, exe := artifactFixture(t)
	stale := exe + ".failed-20261001-000000"
	fresh := exe + ".failed-20261022-120000"
	writeFile(t, stale, "BAD-BUILD")
	writeFile(t, fresh, "BAD-BUILD")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	app.cleanupUpdateArtifacts()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a two-day-old failed copy survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a fresh failed copy was deleted; it is rollback evidence: %v", err)
	}
}

// TestCleanupDropsOldFailedCopiesWhenAConfirmedBuildRuns covers the other half of
// the evidence rule: once the confirmed build is the one actually running, the
// older failed copies no longer protect anything, and all but the newest go.
func TestCleanupDropsOldFailedCopiesWhenAConfirmedBuildRuns(t *testing.T) {
	app, exe := artifactFixture(t)
	newSHA, err := hashFileSHA256(exe)
	if err != nil {
		t.Fatal(err)
	}
	older := exe + ".failed-20261021-100000"
	newer := exe + ".failed-20261022-100000"
	writeFile(t, older, "BAD-BUILD")
	writeFile(t, newer, "BAD-BUILD")
	now := time.Now()
	if err := os.Chtimes(older, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, now.Add(-1*time.Hour), now.Add(-1*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := updateStateSave(app.updateStatePath(), updateState{
		Stage: stageConfirmed, Exe: exe, NewSHA: newSHA, Bak: exe + ".bak",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	app.cleanupUpdateArtifacts()

	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Errorf("the older failed copy survived a confirmed update: %v", err)
	}
	if _, err := os.Stat(newer); err != nil {
		t.Errorf("the newest failed copy was deleted; one is kept as evidence: %v", err)
	}
}

// TestCleanupKeepsFailedCopiesWhileTheUpdateIsUnconfirmed is the negative of the
// previous test: an unconfirmed record still needs its evidence, so nothing goes.
func TestCleanupKeepsFailedCopiesWhileTheUpdateIsUnconfirmed(t *testing.T) {
	app, exe := artifactFixture(t)
	newSHA, err := hashFileSHA256(exe)
	if err != nil {
		t.Fatal(err)
	}
	older := exe + ".failed-20261021-100000"
	newer := exe + ".failed-20261022-100000"
	writeFile(t, older, "BAD-BUILD")
	writeFile(t, newer, "BAD-BUILD")
	if err := updateStateSave(app.updateStatePath(), updateState{
		Stage: stageLaunched, Exe: exe, NewSHA: newSHA, Bak: exe + ".bak",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	app.cleanupUpdateArtifacts()

	for _, p := range []string{older, newer} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("failed copy %s was deleted while the update is unconfirmed: %v", p, err)
		}
	}
}

// TestCleanupRemovesAnOrphanStagedDownload covers the .new rule: a download no
// record names is junk from an attempt that never wrote a marker; one a record
// names is kept for a retry until it is a day old.
func TestCleanupRemovesAnOrphanStagedDownload(t *testing.T) {
	app, exe := artifactFixture(t)
	staged := exe + ".new"
	writeFile(t, staged, "STAGED-BUILD")

	// Orphan: no marker at all.
	app.cleanupUpdateArtifacts()
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("an orphan staged download survived: %v", err)
	}

	// Named by a record and fresh: kept, so a retry in this session works.
	writeFile(t, staged, "STAGED-BUILD")
	if err := updateStateSave(app.updateStatePath(), updateState{
		Stage: stagePrepared, Exe: exe, Staged: staged,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	app.cleanupUpdateArtifacts()
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("a staged download named by the record was deleted: %v", err)
	}

	// Named but a day old: nothing is going to retry it.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(staged, old, old); err != nil {
		t.Fatal(err)
	}
	app.cleanupUpdateArtifacts()
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("a two-day-old staged download survived: %v", err)
	}
}

// removeFailingSys delegates to the real implementation except for paths that
// contain failSubstring, whose removal is refused. It is how the sweep is made to
// hit a locked file without depending on Windows sharing semantics.
type removeFailingSys struct {
	updateSys
	failSubstring string
}

func (s removeFailingSys) remove(path string) error {
	if strings.Contains(path, s.failSubstring) {
		return errors.New("the file is in use by another process")
	}
	return s.updateSys.remove(path)
}

// TestCleanupSurvivesRemovalFailures pins two properties: a refused delete is
// reported through the log rather than failing the sweep, and the remaining
// leftovers are still handled.
func TestCleanupSurvivesRemovalFailures(t *testing.T) {
	app, exe := artifactFixture(t)
	app.sys = removeFailingSys{updateSys: newSysStub(t.TempDir()), failSubstring: ".failed-"}

	locked := exe + ".failed-20261001-000000"
	orphan := exe + ".new"
	writeFile(t, locked, "BAD-BUILD")
	writeFile(t, orphan, "STAGED-BUILD")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(locked, old, old); err != nil {
		t.Fatal(err)
	}

	// Must not panic, and must not stop at the first failure.
	app.cleanupUpdateArtifacts()

	if _, err := os.Stat(locked); err != nil {
		t.Errorf("the locked failed copy is gone; the fake remove did not fail as intended: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("the sweep stopped after a removal failure: the orphan staged download survived (%v)", err)
	}
}
