# CHANGES — VKMDownload 变更记录

> 每次改完代码都在这里记一笔：日期、改了什么、为什么改。
> 发版 / push 前看这份文件写 release notes。

## 2026-10-15 — 0.2.5（已发布，https://github.com/Vick-moo/VKMDownload/releases/tag/v0.2.5）

### 1. 托盘退出后进程残留（bug）
- 症状：关闭窗口最小化到托盘后，点托盘 退出，图标没了但进程还在后台跑。
- 根因：Wails Windows 的 `Frontend.Quit` 会先问 `OnBeforeClose`（`desktop/main.go:47` 绑的 `app.beforeClose`），而 `beforeClose` 在 `MinimizeToTray` 开时永远返回 `true` 拦截，于是托盘的 退出 和 `Quit` binding 都被否决。
- 改动：
  - `desktop/app.go`：`App` 加 `quitFlag atomic.Bool` + `requestQuit()`；`beforeClose` 见到置位直接放行；`Quit()` 先置位再 `runtime.Quit`；`appVersion` 升到 `0.2.5`。
  - `desktop/tray.go`：退出 handler 先 `requestQuit()` 再 `runtime.Quit`（图标清理走 `shutdown/stopTray`）。
  - `desktop/manager_test.go`：加 `TestBeforeCloseQuitIntentLetsTrayExitThrough`。
  - `desktop/wails.json`：`productVersion` 升到 `0.2.5`。
- 验证：`go vet` 通过；`TestBeforeClose*` 通过；`go test ./...`（desktop）全绿。

### 2. 多连接在部分服务器上全 0 B（bug）
- 症状：`dl.fuckingfast.co` 的任务多连接下载全是 0 B，失败信息是 `segmented download failed ... bytes unclaimed`；单连接能下（实测 25 秒 386 MB）。
- 根因：该站无视 Range 尾端——请求 `bytes=0-524287` 却回 `Content-Range: bytes 0-2097151999/2097152000`（body 倒是正确的前缀）。引擎 `validateRangeResponse` 要求尾端精确相等，于是每块都失败；错误是 retryable 所以重试 10 次，最后 `fetchSegment` 吞掉真错只报通用的 `bytes unclaimed`，界面永远 0 B。
- 改动：
  - `internal/engine/download.go`：`validateRangeResponse` 接受 `end >= off+n-1`（只读前 n 字节）；`fetchSegment` 循环尾保留 `lastErr` 并写进报错信息。
  - `internal/engine/download_test.go`：加 `TestDownloadRangeEndIgnoredByServer`（httptest 模拟无视尾端的服务器，多连接逐字节校验）。
- 验证：新测试通过；`go test ./...`（engine，约 31s）全绿。
- 备注：`tasks.json` 里 10 个 `queued` + 0 B 是预期行为（`AutoStart` 关 + `MaxConcurrent: 2`，任务只入列没启动，probe 没跑所以 size 未知），不是 bug。


### 3. restart turns never-started queued into paused (pending)
- New tasks with AutoStart off are queued by design (manager.go Start).
- But loadTasks (manager.go:1167) rewrites queued to paused on restart.
- Harmless: Resume still starts them. Only slightly imprecise semantics.
- User informed; leaving as-is until user decides.
### 4. Plan download refused when concurrency slots full (bug)
- Symptom: with tasks queued and MaxConcurrent slots occupied (e.g. by stuck
  0B downloads), Plan button and Start All appear dead: plan reports no
  waiting tasks.
- Root cause: StartPlan only counted tasks newly started by that call
  (manager.go StartPlan). Slots full means started==0, so it errored and the
  watcher never launched. Turning everything paused would NOT fix it: gated
  tasks stay queued either way.
- Fix: active + queued counts as a valid plan; watcher launches and pumpQueue
  pulls queued tasks in as slots free. Returns covered count for the UI.
- Test: TestStartPlanWithFullSlotsStillRuns. Existing plan tests unaffected.

### 5. Deleting a record leaves junk in Downloads (fixed)
- Symptom: deleting tasks from the list left .part files behind (13 orphans
  with URL-token names in Downloads). Two sources: engine never cleaned up
  after failed downloads, and record deletion never touched disk.
- Decision with user: deleting an UNFINISHED task auto-removes its .part +
  sidecar (they can never resume without the record). Finished files are
  never auto-deleted. Same rule flows into bulk delete and ClearFailed.
  Re-adding the same URL restarts from zero.
- Changed: desktop/manager.go Remove, README delete section, App.tsx confirm
  texts (x3), TaskRow trash title, new test TestRemoveDropsUnfinishedPartials.
- Verified: new test passes, go vet clean, tsc clean.

### 6. Extension hands over empty filename (bug, fixed)
- Symptom: files grabbed from the browser land in Downloads named after URL
  tokens with no extension (e.g. 2GB fuckingfast files unrecognizable).
- Root cause: extension/background.js onCreated handler handed over
  item.filename from creation time, when Chrome usually does not know it yet.
  The 250ms wait for size was not reused for the name.
- Fix: re-search the download item after the size wait and hand over the
  fresh filename when present. Only helps servers sending Content-Disposition
  and only future downloads; already-landed token files need manual rename.
- Verified: node --check clean. Needs extension reload in browser to take
  effect (unpacked extension: chrome://extensions reload button).

### 7. Probe ignores Content-Disposition from ranged GET (bug, fixed)
- Symptom: fuckingfast files land with URL-token names even though the server
  sends Content-Disposition with the real name (RFC 5987 filename* form).
- Root cause: probeHead fills SuggestedName from the URL token when HEAD has
  no disposition; probeRange then refused to recompute it (guarded by == "")
  even after learning the real disposition. The probe knew the name but never
  used it. Manager already adopts probe names on first progress, so no UI
  change needed.
- Fix: probeRange promotes a disposition-derived name over the URL fallback.
- Verified: new test TestProbePrefersDispositionOverURLToken passes; live
  `vdl info` on a token URL now reports the real .rar name.
- Side finding: these dl links are single-use/expiring (a used one 404s).
  Start remaining parts promptly.

### 8. New tasks paused by default + duplicate URLs rejected
- New tasks added without Start now rest PAUSED (was queued): clicking one
  task starts only that one, the rest never auto-follow. Start All / plan
  still pick up paused tasks, so bulk flows are unchanged.
- Same URL re-added to the same folder is rejected with the existing record
  (no second transfer fighting over one .part, no burnt single-use tokens).
  Same URL to another folder is allowed; completed re-downloads allowed.
- Extension: duplicate handover answers 200 (no browser fallback double
  download) with an already-listed notification.
- Dialog: on duplicate it closes, selects the existing row and flashes.
- Tests: TestNewTasksRestPaused, TestDuplicateURLIsRejected. Full desktop
  suite green with no old-test changes needed.
- Verified: go vet clean, tsc clean, wails build green (bindings regenerated).

### 9. Add-time probe: instant names+sizes, content-based duplicates
- New tasks with no browser filename are probed once at add time (10s cap,
  fail-open, 1 retry): the row shows the real name and size immediately
  instead of blank/0B, and the probe powers duplicate layer two.
- Layer two: different URLs, same host + filename + compatible size are the
  same file (single-use token links) and re-adds are rejected. Identical
  URLs stay on layer one (another folder is allowed); completed exempt.
- Dead links cost ~4s on add (then created unnamed as before); live links
  ~0.5s. Transfer re-probes anyway, so add-probe retries stay minimal.
- Tests: TestSameFileDifferentURLIsRejected. Full desktop suite green.

### 10. Add-time probe fast-fail for dead hosts
- The 4s per dead-link add was our own retry sleeps, not the server.
- Fix: 2s TCP dial pre-check (hostReachable); unreconnectable hosts skip the
  probe entirely. Live links unaffected. Proxy users keep old behaviour
  (dial bypasses proxy, fail-open).
- Result: probe-related tests 20s -> 4.5s; full desktop suite 89s -> 21s.
- Test: TestHostReachableFastFails. Exe rebuilt.

### 11. Ask-location dialog hidden behind browser (fixed as far as possible)
- Symptom: with ask-location on, the placement dialog opened behind the
  browser; user had to minimise the browser to find it.
- Fix: new BringToFront binding (Unminimise + Show + always-on-top flash)
  called when ask/clipboard dialogs open. Wails v2 is single-window, so a
  detached small window is NOT possible; whole window comes forward with the
  modal dialog. Alternative offered: turn off ask-location for zero popups.
- Verified: vet + tsc clean, wails build green. Foreground behaviour needs
  human verification (Windows may still only flash the taskbar).

### 12. Multi-select bulk actions (done, field editing still pending)
- Toolbar shows when rows are selected (click, Ctrl+click to add):
  continue / pause / delete-selected with per-task settle counting, plus
  clear-selection. Delete follows the unfinished-partials rule.
- Uses existing per-task bindings; no backend change. Original TODO's
  batch field editing (connections/destination) still open.

### 13. Selection UX redo: checkbox + scope-switching buttons
- Per user: always-visible checkbox at the right of each name (no edit mode);
  row click / Ctrl+click still work.
- Top buttons switch scope when rows are checked: start/stop/delete act on
  the selection with counts in labels; plan stays all-tasks. Removed the
  extra selection bar. Delete-visible also clears stale selection.
- Verified: build + tsc clean.

### 14. Scoped plan download + checkbox left (per user)
- Plan button joins the scope switch: with checked rows it runs
  StartPlanSelected (only those ids started and watched; removals and
  cancels count as intervened and skip end actions). No selection means
  the global plan as before. finishPlan refactored into finishPlanEnd.
- Checkbox moved to the LEFT of the name (convention; user asked to be
  stopped on illogical ideas, rule added to both AGENTS.md).
- Fixed two stale comments (ClearFailedTasks/RemoveTasks records-only).
- Test: TestStartPlanSelectedCoversOnlyTheSelection. Full suite green.

### 15. Delete missed dir-destined partials (bug, fixed)
- Symptom: every delete flavour left .part/.json behind for tasks created
  without a filename. Root cause: Remove/Cancel deleted dest+suffix, but
  those tasks keep Dest as the DIRECTORY while the engine resolves
  dir+server-name underneath, so the deletion pointed at a nonexistent path.
- Fix: partialPaths() mirrors the engine resolution (dir+view.Name when
  Dest is a directory); used by Remove and Cancel. User's 4 orphan files
  deleted manually (records long gone, unreachable by code).
- Test: TestRemoveFindsPartialsByServerName. Full suite green, exe rebuilt
  and app restarted.

### 16. Start button looked dead on slow links (fixed)
- Symptom: clicking start-all changed nothing on screen for seconds.
- Root cause: startRun changed status but emitted no event; UI waited for
  the engine's first progress (seconds away on slow links).
- Fix: startRun emits task:progress synchronously on every path
  (downloading, queued-behind-limit, update-parked). Covers single,
  bulk, plan and pumpQueue starts.
- Test: TestStartEmitsProgressImmediately (asserts 2 downloading + 1 queued
  events with zero waiting). Also caught and fixed a test-side deadlock
  (never hold a lock across StopAll: its paths emit too). Full suite green,
  exe rebuilt + restarted.

### 17. UI refresh rate setting (fast/medium/slow)
- New setting in Settings: fast 250ms (today's behaviour, default),
  medium 650ms, slow 1000ms. Controls engine progress-report interval only;
  transfer speed untouched; applies to newly started runs.
- Wiring: Config.RefreshRate + normalize + refreshInterval() in run();
  select dropdown in SettingsPage. Old configs without the key fall back
  to fast (no visible change on upgrade).
- Test: TestRefreshIntervalMapping. vet/tsc/build/tests green, app restarted.

### 18. Queued rows can now pause + speed-limit self-check documented
- Queued rows showed only resume; backend Pause always accepted queued.
  Frontend now shows pause on queued rows too (stops auto-queue), plus
  space-shortcut support. No backend change.
- User speed report diagnosed as self-imposed: speedLimitKb=1000 caps the
  total at ~1MB/s (matches the 1.1MiB/s screenshot exactly). Fix is settings
  change to 0, not code. part01 0B row has a dead (expired) link: re-click
  from the site for a fresh token.
- Verified: build + tsc clean, app restarted.

### 19. Settings apply live: concurrency + speed (no restart needed)
- Raising MaxConcurrent pulls queued tasks in at once (SetConfig pumps only
  on increase). Lowering never kills running transfers; the queue gate
  honours the new value by itself.
- Speed limit retunes the shared limiter in place (new Limiter.SetRate):
  running transfers feel it on their next block. Unlimited included.
- Fixed a latent race found on the way: progress snapshots taken outside
  the lock could be delivered out of order (TestProgressIsMonotonic failed
  on a clean tree too). Emission now clamps to a delivery watermark.
- Tests: TestLimiterSetRateAppliesLive, TestApplySpeedLimitRetunesLive.
  Engine + desktop suites green incl. -race on progress/limiter. Exe rebuilt
  and app restarted.

### 20. Refresh rate applies live too
- SetConfig now pushes the cadence to every running transfer
  (livedl registry in Manager + Downloader.SetProgressInterval; trackers
  read the atomic value per block). No restart, no re-probe.
- Tests: TestDownloaderProgressIntervalLive,
  TestApplyRefreshIntervalReachesLive. Engine + desktop green incl. race.
  Exe rebuilt and app restarted.

### 21. Column drag ripples to other dividers (bug, fixed)
- Symptom: dragging one header divider moved other columns too.
- Root cause: the borrow loop spilled to further neighbours once the
  adjacent one hit its minimum, changing the total and triggering the
  proportional rebalancer across all columns.
- Fix: a divider moves only its two sharing columns by equal and opposite
  amounts (growth bounded by what the neighbour can surrender); total stays
  constant so layoutWidths is a no-op and every other divider is untouched.
- Verified: build + tsc clean, app restarted. Needs human drag check.

### 22. Column drag blinks past the stop (fixed)
- Symptom: holding the drag past a column's limit flickered the list.
- Root cause: every mousemove issued setPrefs (new array identity) even
  with zero change, repainting the list and rewriting localStorage at
  pointer rate.
- Fix: early return when the clamped width equals the current one.
- Verified: build + tsc clean, app restarted. Needs human drag check.

### 23. Size column too narrow
- Widened size/added-time column default 104->124, minimum 84->104.
  Note: widths persist in localStorage, so existing installs keep old
  widths until a divider is double-clicked to reset. Exe rebuilt+restarted.

### 24. Column drag snaps back at the limit (bug, fixed)
- Symptom: pulling past a column's stop made the edge jump back and blink.
- Root cause: the growth cap mixed the drag-start base with the LIVE
  neighbour room, so the cap collapsed to the start value the instant the
  neighbour ran out - one frame snaps the column back, the next fights
  forward again.
- Fix: clamp the absolute pointer target against current-value bounds only.
  At the stop the target equals the current width and the early return
  holds it still.
- Verified: build + tsc clean, app restarted. Needs human drag check.

### 25. Column drag cascades through pinned neighbours
- Request: dragging the divider left of a column that needs space should
  automatically push pinned columns further along, no manual pre-moving.
- Behaviour: dragging a boundary moves it; the growing side takes, the
  shrinking side gives nearest-first down to minimums; the far side is
  never touched and the total stays constant (rebalancer stays out).
  Exhausted side means the edge stops; same-array return skips renders.
- Verified: build (incl. tsc via vite) + tsc clean, app restarted. Human
  drag check needed.

### 26. Column drag runaway (bug, fixed)
- Symptom: a small drag flew the divider to the screen end.
- Root cause: the clamped delta (absolute from drag start) was ADDED onto
  live state every move instead of recomputed from the drag-start base -
  quadratic accumulation.
- Fix: snapshot prefs at mousedown; every move rebuilds from that base.
  1:1 tracking restored.
- Verified: build + tsc clean, app restarted. Human drag check needed.

### 27. Helper files hidden in Explorer
- .part + sidecar are now created with the Windows hidden flag, so the
  download folder shows only finished files (same idea as system files).
- Rename preserves the flag, so publish explicitly unhides the finished
  file - otherwise completed downloads would stay invisible. No new
  dependency (stdlib kernel32 via build-tagged files; no-ops off Windows).
- Test: TestHelperFilesHideAndPublishUnhides. Engine suite green,
  linux/darwin builds verified. Exe rebuilt + app restarted.

### 28. Finished rows flipped back to downloading (bug, fixed)
- Symptom: tasks complete on disk and in tasks.json, UI stuck at 100%
  downloading until clicked. Backend exonerated by evidence twice.
- Root causes, both event ordering: (1) overlapping full-list reloads with
  no generation guard let a stale fetch paint over task:done; (2) progress
  travels a queue while done jumps it directly, so late stale progress
  resurrected finished rows.
- Fix: reload applies only the latest call; progress handler ignores
  non-completed updates for completed rows (failed excluded: resume
  legitimately returns them to downloading).
- Verified: build + tsc clean, app restarted.

### 29. Optimization batch (5 items)
- Async add-time probe: Start returns at once, probe fills name/size via
  event; untouched probed duplicates remove themselves (fillProbedName).
- Bulk index writes coalesced: batch() makes StartAll/StopAll/RemoveTasks/
  RemoveFailed/StartPlanSelected write tasks.json once (kept Indent: humans
  read this file when diagnosing).
- Progress pump keeps newest on saturation instead of dropping it.
- TaskRow memoized (+ stable onChanged); update check single-sourced to the
  mount query (event subscription removed).
- Also fixed: progress out-of-order delivery watermark (pre-existing race,
  failed on clean tree too), racy test assertion, TempDir flakes noted.
- Verified: desktop + engine suites green, race on progress/limiter clean,
  tsc clean, exe rebuilt + restarted.

### 30. Resume-flush knobs: per-MB, per-time, per-percent
- Settings has three independent sidecar knobs (KB / ms / % of total, 0
  disables the percent one): whichever fires first writes. Report and save
  throttles are fully split in the tracker now.
- Debugging footnote: two cancel/resume tests failed mid-way; root cause was
  testOptions normalizing BEFORE setting ProgressInterval, pinning save
  thresholds to the 250ms default while reporting ran at 50ms. Production
  run() always sets every field explicitly - unaffected. Fixed the fixture.
- Tests: extended TestRefreshIntervalMapping, TestSaveThrottlesIndependently
  (percent case). Suites green, tsc clean, exe rebuilt + restarted.

### 31. Settings tabs + flush switches + reorder
- Settings page is tabbed (Chrome-tabs style): download / extension / plan /
  clipboard / done / window / update. Sections stay mounted so drafts survive.
- Download tab grouped: parallel / speed / display / resume-flush; ask-location
  and organize sink to its bottom. Extension up, clipboard down, update last.
- Resume-flush triggers are individually switchable (any subset, at least one
  forced on); numbers disable with their switch.
- Footnote: block moves via script broke JSX twice (truncated take + brace
  drift); repaired against git pristine, verified by counts + tsc.
- Verified: tsc clean, build green, app restarted.

### 32. Flush checkbox restyle + swap
- Number box left, checkbox right; checkbox redrawn for the dark theme
  (accent fill + white tick) instead of the native box.
- Verified: tsc + build clean, app restarted.

### 33. Flush rows redesigned (subtitle + switch, box below, hint last)
- Per user layout: sub-title row with the on/off switch at right, number box
  on the next row, description under it. Uses the existing Switch component.
- Verified: tsc + build clean, app restarted.

### 34. Flush cards per mockup
- Trigger cards: group card with accent-bar title, per-trigger card with
  title + number + switch right, hint below. Removed the checkbox restyle
  dead CSS. Verified: tsc + build clean, app restarted.

### 35. Flush rows back to native design language
- The card mockup was layout reference only; restyled into plain .setting
  rows (title + hint left, number + switch right), matching every other row.
  Removed the flush card CSS. Verified: tsc + build clean, app restarted.

### 36. UI small fixes (UNRELEASED, UI only)
- Row switches vertically centered against number boxes.
- Clipboard interval hint notes monitoring must be on first.

### 37. Concurrent-duplicate guard + extension size wait (UNRELEASED)
- Manager: url+dest inflight claim in Start, so same-instant /add arrivals
  yield one task and duplicates (was: N tasks for N racers, seen 3x same URL).
  Regression test TestStartConcurrentSameURL (fails 16-vs-1 without fix).
- Extension: sizeKb polls up to ~1.5s for totalBytes before answering unknown,
  so the min-size filter sees real sizes instead of bypassing on -1.

## TODO (user requested, work later)

- Detached ask-location popup WITHOUT v3 migration (backup plan).
  Idea: tiny companion program (Go stdlib + native dialog, tens of KB) that
  shows URL/dir/filename/connections and POSTs back to the main app's
  loopback API. Main app stays in tray; helper launched on ask arrivals.
  Pros: detached small window now, no migration. Cons: second component to
  maintain, plain styling, edge cases (e.g. helper with no app running).
  Requested 2026-10-15 as an option. Not started.


- Detached ask-location dialog in its own small window (no main window).
  WHY v3: user wants only the small placement dialog to pop when a browser
  download arrives, without the whole main window coming along. Wails v2 is
  single-window so the modal must live in the main window (bring-to-front,
  CHANGES #11, is the current ceiling); only Wails v3 has multi-window.
  STATUS (2026-10-15): parked on purpose. v3 is still beta (APIs unstable),
  and migration is rewrite-scale (window model, JS bridge, bindings,
  packaging all change; manager/tray/update/clipboard/loopback/React all
  need porting). Frameworks reviewed: Electron (needs Go engine as sidecar
  + huge bundle), Tauri (Rust rewrite), Fyne/walk (React UI rewrite).
  Verdict: stay on v2 until v3 goes stable - Go code moves over almost
  untouched then. Revisit only when the small window becomes a must.


- Multi-select + multi-edit tasks in the list. Bulk start/stop/remove DONE (2026-10-15, CHANGES #12). Batch field editing (connections/destination) still open.


### 3. restart turns never-started queued into paused (pending)
- New tasks with AutoStart off are queued by design (manager.go Start).
- But loadTasks (manager.go:1167) rewrites queued to paused on restart.
- Harmless: Resume still starts them. Only slightly imprecise semantics.
- User informed; leaving as-is until user decides.
## 2026-10-14 — 0.2.4（已发布）
- 见 `release-notes-0.2.4.md`。
