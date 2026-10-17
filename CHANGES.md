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

### 38. Extension 1.0.2 store update (pushed, store review pending)
- Manifest 1.0.1 -> 1.0.2 (update submissions need a higher version).
- Store zip rebuilt with the sizeKb fix; package replaced on Partner Center,
  review clock restarted.
- #37 commits pushed to main; no GitHub release yet.

### 39. Engine correctness batch (UNRELEASED)
- fetchOnce commits actual written bytes (was: whole block), restoring the
  split/outstanding invariant. Regression: TestCommitPartialKeepsClaimOutstanding.
- Size<0 barred from segmented path (was: fake-success empty file).
- Segmented completion always verifies known length; VerifySize now only
  controls extra checks.
- Disk write errors (PathError/SyscallError/ShortWrite) no longer retried.
- Sidecar watermark advances only on successful save (+saveFails counter).
- Limiter bucket scales with rate (small limits actually limit now).
- debugFile closed in Downloader.Close; ChunkSize honored on segmented path;
  NoRange branch made explicit error. 9 new tests in repair_test.go.

### 40. Backend persistence batch (UNRELEASED)
- tasks.json persists manual/referer/cookie/userAgent (restart no longer
  kills private tasks or mis-organizes manual ones).
- paused/failed count as plan-intervened (no more surprise shutdown/sound).
- organize falls back to copy+remove cross-volume, uniqueDest (1) suffix,
  failures surface in task.Error.
- Optional pairing auth: ApiToken (auto-generated) + ApiAuthRequired
  (default off); Bearer guard on /add + settings POST.
- /add field caps (URL/Referer 8KB, filename 255 rune); content-layer
  duplicate re-check in insert critical section + pickDuplicateVictim.
- saveMu + fsync; corrupt tasks.json preserved as tasks.json.corrupt-*;
  completed only flipped on IsNotExist; seq reseeded from loaded IDs;
  clipboard URL trim shared helper; debugLog mutex; StartPlanSelected
  counts real resumes; log URLs redacted to host+path. 15 new tests.

### 41. Frontend UX batch (UNRELEASED)
- files:dropped wired (drag a link opens the dialog); update:available
  subscribed (offline-start banner no longer lost) + release-page button.
- Dialog arrivals queue instead of overwriting; selected + keyboard delete
  unified behind ConfirmDialog; selected pruned against live tasks.
- canceled filter/sidebar entry; clipboard switches made mutually exclusive;
  minSegmentSize setting added; min-size now raw KB input; empty organize
  rows blocked with a hint; per-tick scans merged to one pass.
- Column first-frame guard, width cap proportional, sidebar width persisted;
  ConfirmDialog Esc/mask close; empty-filter back button; row delete confirm;
  row errors flash. vitest harness: 22 tests (format/columns/filterLabel).

### 42. Extension no-loss batch (UNRELEASED, needs browser reload)
- Recognizes pending (no longer fake-success); POST-before-cancel ordering;
  completed browser items left alone; fallback uses fresh filename.
- ownDownloads mirrored in chrome.storage.session (best-effort: manifest
  keeps 4 permissions for the pending store review); fallback try/catch.
- Pairing token field in popup (Bearer header, 403 shows pairing hint);
  fresh policy on status; selection-text CJK punctuation stripped.
- node:test suite: 22 tests (decision + manifest contract locks).

### 43. Release + docs batch (UNRELEASED)
- release.ps1 preflights: existing tag / dirty tree / version-replace hits.
- CODEMAP section 4 rewritten to actual throttle/flush behavior, dup entry
  merged, section 6 covers extension + release flow, handover order fixed.
- CHANGES TODO deduped. update_test.go: offline-silence + allowlist tests.

### 44. Peer process verification (UNRELEASED, default on)
- Loopback now confirms the caller is a genuine signed browser (TCP owner
  PID + exe allowlist + Authenticode, cached per exe) before honoring /add
  and /settings. Forged Origin curl is rejected; failures logged with exe.
- Settings switch (top of extension section) with ms cost in the hint
  (~1ms per download, ~300ms once per new browser exe). Reads live config:
  toggle applies instantly, no restart. Lookup infra failures fail open.
- 8 new tests (allowlist, admit/reject/signature/fail-open, end-to-end
  200/403, cache, real-sig manual-only after a Defender false positive).

### 45. Pairing token removed everywhere (UNRELEASED)
- Peer check made the token redundant (same-user malware reads both copies),
  so it is gone: backend fields/bindings/guard, settings pairing section,
  popup token box, background Bearer + pairError paths, PRIVACY row.
- Popup keeps version line + refused-error display; manifest keeps storage
  (ownDownloads session mirror still uses it). Net -439 lines.

### 46. Recheck fixes round (UNRELEASED, self-fixed after read-only audit)
- Peer: WTD_STATEACTION_CLOSE=2 (was 5, leaked handles), empty-table panic
  guard, revocation-off documented, single-Stat cache with normalized capped
  keys, exe path buffer + error wraps, dup comments merged.
- Extension: failure fallback cancels the original first (was: double
  browser download); toggle reads fresh policy (was: stale-cache double
  flip). New cancel-before-download regression test (node 22 total).
- Frontend: arrival race via dialogOpenRef; NewTaskDialog resets on all
  initials; runSelected in-flight guard; dead clearFailedTasks wrapper out.
- Docs: README 4-setting table + peer-check security section; PRIVACY
  cookie scope + notifications/session rows; Draft comment fixed.

### 47. 性能优化批（UNRELEASED，只读性能审计后自查自改）
- 引擎：ReserveSize 默认 512KiB→16MiB、上限钳 64MiB（高延迟源不再被"每 512KiB 一个 RTT"卡死单 worker ~10MB/s；claim 是记账不是内存，读仍按 ChunkSize 走）；ChunkSize 默认 128KiB→256KiB（与池块对齐，每字节的 WriteAt/commit/限速税减半）；分段下载启动时 Truncate 预分配到完整大小（高段写入不再按需零填空洞、碎片减少，尽力而为失败不阻断）；sidecar 改紧凑 JSON；splitLargest 改切"claim 之后的未认领区"（旧规则要求无在途 claim，而 worker 全程持 claim，动态分片实际从未触发过；新切点保证 claim 全留在头段）；进度回调改 TryLock 丢弃 + finish() 走阻塞 emitFinal（慢 UI 不再串住 worker，终帧 100% 必达）。测试更新：claim 内不越界切分、全被 claim 时拒绝切分。
- 后端：pumpQueue 单快照（原来每启动一个排队任务都全表 List+activeCount，StartAll 上百任务 O(N²)→O(N logN)）；loadTasks 的 completed Stat 挪出 m.mu（断网盘卡 Stat 不再冻结 List/Stats/Start）；List 改单次管理器锁；新增 countByStatus 一遍扫描（Stats/计划观察器 500ms 轮询不再全表拷贝+排序，statsIntervened 并入 taskCounts.intervened）；批量删除合并为单 waiter goroutine + 一次 pumpQueue（原来每行一个 3s waiter 各自唤醒扫全表）；同内容查重按任务缓存 host（add 高频不再每候选 url.Parse）；文件名探测加 4 并发信号量（扩展 burst 添加不再几十个并发 dial）；进度泵饱和改整队倾泻（不再回放旧位置造成进度回跳）；run() 选项构造里贴了两遍的 ResumeFlush 块删除。
- 启动：tray/文件拖放/更新检查提前到 loadTasks 之前；config.json 只在文件缺失时写（原每次启动无条件重写+触发杀软实时扫描）；恢复完成后发 tasks:loaded 事件、前端收到即 reload（修"首屏 listTasks 撞上恢复中拿到空表且无人纠正"的竞态）；release.ps1 构建 -trimpath（去 exe 内本机路径，可复现构建）。
- 体积：third_party/systray 内置 fyne.io/systray v1.12.2 单补丁副本——上游 systray_notifier_unix.go 缺 //go:build 约束（_unix 后缀不是隐式平台约束），把 Linux-only 的 godbus/dbus + notifier 链进每个 Windows 构建。desktop/go.mod replace 指向本地副本。实测本地 exe 13,273,600→12,909,568 B（-364,032 B），二进制内 godbus 串 506→0。见 third_party/systray/PATCH.md。
- 前端：TaskList 排序预计算每行 key + Intl.Collator 模块单例 + failed 置顶并入同一次比较（原来两趟 sort）；顺序指纹缓存——progress tick 不再每 250ms 全表 O(n log n) localeCompare 重排（不变序时映射旧序到新对象，行照常刷新）；分组一次遍历同时产出各组 exts（原来每组各扫全表）；列宽/侧栏宽度 localStorage 防抖 300ms（拖拽不再逐帧写盘）；keydown 全局监听改 ref 单次订阅（原来每次 tick 重挂）；formatWhen 的"今天"按分钟缓存。
- 验证：go vet 全过；engine go test 全绿；desktop go test 除基线已知 TempDir 清理竞态（轮换 victim，单测隔离全过）外全绿；tsc 0 错；vitest 22/22；扩展 node 测试未动本轮无改。新 exe 已构建并重启（本地实测 PID 2084，日志干净）。
- 有意不做并记录原因：ReserveSize 自适应（先吃默认值 80% 收益）；debugf 环境变量缓存（与测试 t.Setenv 生命周期冲突）；saveTasks 紧凑 JSON（Indent 是诊断可读性、故意的，且突发写量受 maxConcurrent 限界）；任务列表虚拟化（当前规模用不上）；Sidebar memo（内联 lambda 会抵消收益）；探测完成校验彻底异步化（保持语义不变，只把 Stat 挪出锁）；前端 gzip 资产、更新检查延迟（收益/成本比不足）。

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

## 2026-10-14 — 0.2.4（已发布）
- 见 `release-notes-0.2.4.md`。
