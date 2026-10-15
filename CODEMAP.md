# CODEMAP — VKMDownload AI 维修地图

> 给 AI 看的索引，不是文档复读。修 bug 时先看这张表定位文件，再读源码。
> 约定：`file:line` 行号以当前 `main` 为准，改动后可能漂移，用 grep 二次确认。

## 0. 一句话架构

`internal/engine`（多连接下载引擎）+ `desktop`（Wails Go 后端 + React 前端）+ `extension`（Chromium 扩展，经回环 HTTP 与应用通信）+ `testsrv`（故障注入测试服务器）。

## 1. 目录职责

| 目录 | 职责 | 入口 |
|---|---|---|
| `internal/engine/` | 探测、规划分片、并发下载、续传、重试、限速 | `download.go:Download` |
| `testsrv/` | 可编程恶劣服务器（假 Range、断流、谎报长度、403、chunked） | `server.go` |
| `cmd/idm/` | CLI：`get` / `info` / `bench` | `main.go` |
| `cmd/testsrv/` | 独立运行的测试服务器 | `main.go` |
| `desktop/` | Wails 后端：任务状态机、设置、剪贴板、回环 API | `app.go:NewApp`、`manager.go:Manager`、`api.go`、`config.go` |
| `desktop/frontend/src/` | React 界面：列表、设置、新建任务 | `App.tsx`、`api.ts`、`components/` |
| `extension/` | 浏览器扩展：拦截下载、右键菜单、策略轮询 | `background.js:findApp`、`handOver` |
| `design/DESIGN.md` | 界面设计规范，改界面前先读 | — |

## 2. 关键文件索引

### 引擎（`internal/engine/`）

| 文件 | 管什么 | 修什么先看它 |
|---|---|---|
| `download.go` | 下载主梯（segmented+if-range → segmented → single）、`fetchSegment`/`fetchOnce`、`publish`、`progressTracker`、`debugf` | 下载失败、数据损坏、进度不动、发布逻辑 |
| `probe.go` | HEAD + Range 实测探测，不信 `Accept-Ranges` 头 | 误判服务器能力、登录页识别（`ErrNeedsSignIn`） |
| `segment.go` | 分片规划 `planSegments`、动态切分 `splitLargest`、`reserve/commit/unreserve` | 并发空洞、进度计数对不上 |
| `state.go` | sidecar 读写（`PartSuffix=.vkmdownload.part`、`StateSuffix=.vkmdownload.json`）、`matches` 严格校验 | 续传失败、版本不匹配 |
| `transport.go` | 强制 HTTP/1.1（多连接加速的前提） | 速度异常、连接复用问题 |
| `limiter.go` | token bucket 全局限速 | 限速不准、取消后 timer 堆积 |
| `backoff.go` | 指数退避 + `isRetryable` 分类 | 重试过多/过少、429/5xx 处理 |

### 桌面后端（`desktop/*.go`）

| 文件 | 管什么 | 修什么先看它 |
|---|---|---|
| `manager.go` | `Manager` 状态机：Start/Pause/Resume/Cancel/Remove、批量操作、计划下载、`Stats`、`loadTasks/saveTasks` | 任务卡住、暂停变取消、批量卡死、计数对不上 |
| `update.go` | 更新检查：`CheckUpdate` binding、`isNewerVersion` 对比、`checkUpdateOnce` 启动通知、`OpenReleasePage` | 更新横幅不出现、版本号比错 |
| `tray.go` | 托盘图标（fyne.io/systray，自带消息循环）：显示窗口/退出，左键恢复 | 关窗口窗口丢失、托盘无图标、托盘退出后进程残留 |
| `config.go` | 设置结构（含 `AskLocation`、`Organize*`、`UpdateStatus` 相关）+ `organizeTarget` 落点计算 | 设置不生效、整理落点不对 |
| `app.go` | Wails 生命周期、binding、剪贴板轮询、托盘关闭语义、quitFlag 退出意图 | 启动崩溃、关不掉窗口、剪贴板重复建任务 |
| `api.go` | 回环端点 `/ping` `/settings` `/add`、`apiGuard`、`sanitizeSuggestedName` | 扩展连不上、文件名越界 |
| `config.go` | 设置结构、默认值、`normalize`（含 `MaxConcurrent` 校验） | 设置不生效、默认值异常 |

### 前端与扩展

| 文件 | 管什么 |
|---|---|
| `frontend/src/api.ts` | binding 封装 + 事件订阅（`on` 取消语义注意） |
| `frontend/src/App.tsx` | 界面骨架与 6 个事件订阅 |
| `frontend/src/components/TaskList.tsx` | 列表 + 列宽记忆（键 `vkmdownload.column-widths`） |
| `frontend/src/components/NewTaskDialog.tsx` | 新建任务输入（前端无路径净化，靠后端） |
| `frontend/src/components/SettingsPage.tsx` | 设置页 |
| `extension/background.js` | `findApp` 端口扫描 18888–18898、`handOver` 先删后送、`nameFromUrl` |
| `extension/manifest.json` | 权限（`downloads` + `contextMenus` 必需，`storage` 当前无调用） |

## 3. 改某功能该看哪

| 想改 | 看这些 | 测试 |
|---|---|---|
| 分片/动态切分 | `segment.go` 全文件 + `download.go:runSegmented` | `go test ./internal/engine -run 'Segment|Split' -v` |
| 续传一致性 | `state.go:matches`、`download.go:loadResumable/saveState` | `go test ./internal/engine -run 'Resume\|Pause' -v` |
| 重试/退避 | `backoff.go`、`download.go:fetchSegment/doWithRetry/isRetryable` | `go test ./internal/engine -run 'Retry\|Backoff\|Probe' -v` |
| 限速 | `limiter.go` + `download.go` 内两处 `Limiter.Wait` | `limiter_test.go` |
| 任务状态机 | `manager.go:Start/Pause/Resume/Cancel/Remove*` + `stopIntent` | `cd desktop && go test ./...` |
| 设置项 | `config.go` + `SettingsPage.tsx` + `types.ts`（Go 改了要同步） | 改设置→重启→还在 |
| 剪贴板 | `app.go:clipboardLoop/offerClipboardURL` | 复制 URL 看预期行为 |
| 扩展接管 | `api.go` + `background.js:findApp/handOver/getPolicy` | 先启动应用再加载未打包扩展 |
| 进度/速度显示 | `download.go:progressTracker` + `TaskRow.tsx` | 本地 testsrv 限速对比 |

## 4. 关键流程速查

- **下载梯**：`download.go:Download` → 有 sidecar 则 `segmented+if-range` → 可分片则 `segmented` → 兜底 `single`。`ErrIfRangeIgnored` 表示换策略，不是传输失败。
- **进度节流**：`250ms / 1MiB` 先到先发（`download.go:newTracker`），`emit` 内先落盘 sidecar 再回调。
- **发布顺序**：`publish` 先 `Sync+Close` → `moveFile` → `track.finish()` → 删 sidecar。删早了会被 `finish` 复活。
- **任务持久化**：`tasks.json`（GUI 意图）与 sidecar（引擎真相）分离，索引坏了不影响续传。
- **扩展握手**：扩展轮询 `127.0.0.1:18888–18898/ping`，认 `app=="VKMDownload"`；每次拦截前 GET `/settings` 取策略。应用必须先启动。

## 5. 命名与环境（统一小写 `vkmdownload`）

- module：`vkmdownload`，桌面 `vkmdownload/desktop`，`replace vkmdownload => ../`
- 后缀：`.vkmdownload.part` / `.vkmdownload.json` / `.vkmdownload.tmp`
- 环境变量：`VKMDOWNLOAD_DEBUG`（`=1` 进 stderr，`=路径` 进文件）、`VKMDOWNLOAD_DIAGNOSE`（压力诊断）、`VKMDOWNLOAD_LEGACY_ACCOUNTING`（回归对照）
- 数据目录：`%APPDATA%\VKMDownload\`（`config.json`、`tasks.json`、`vkmdownload.log`）。
  早先的 `migrateLegacyDataDir`（从 `VKDownload`/`VDownload` 搬运 `config.json` 与 `tasks.json`）
  已移除。影响仅限"装过 0.2.2 之前的版本、之后直接跳到当前版"的安装 —— 那种情况下任务列表与设置
  不会自动搬过来。0.2.2 起的所有版本都已经写进 `VKMDownload`，新装也直接用它。需要恢复的话，
  `git show 7dde868:desktop/app.go` 里有完整实现。
- 列宽键：`vkmdownload.column-widths`

## 6. 常用命令

```bash
go build ./...                       # 根模块构建
go vet ./...                         # 根模块静态检查
go test ./...                        # 根模块全部测试
go test -race ./...                  # 并发正确性（推荐）
VKMDOWNLOAD_DIAGNOSE=1 go test ./internal/engine -run Diagnose -v
cd desktop && go test ./...          # 后端测试
cd desktop && wails build            # 产出 build/bin/VKMDownload.exe
```

## 7. 已修好的坑（历史记录，备查）

1. `download.go` 单流缓冲 panic：`ChunkSize` 已在 `Normalize` 与使用点双重 clamp 到 `bufPoolBlockSize`。
2. `MaxConcurrent` 只校验不排队：已实现 `startRun` 门控 + `pumpQueue` 调度，`StartAll` 只计入真正启动的任务。
3. `Start`/`Resume` 与 `run` 之间的 `done==nil` 窗口：`done` 改由 `startRun` 在同一把锁内创建并传入 `run`。
4. `apiGuard` 放行空 `Origin`：写操作（`/add`、POST）现在必须带浏览器扩展 `Origin`，只读端点与预检不受影响。
5. `emit` 在锁内同步落盘：`save` 已移到 `emitMu` 之外；`debugf` 改用按目标缓存的 append 句柄；`Limiter.Wait` 改用 `NewTimer+Stop`。
6. `debugLog` 无轮转：超过 2 MiB 转到单备份 `.1`。
7. `humanError` 子串误报：状态码只认 `HTTP 403`/`status 403`/`(403)` 形式，磁盘与网络匹配改为大小写不敏感并补全措辞。
8. 计划下载在取消后仍关机：`finishPlan` 在有取消任务时跳过收尾动作。
9. 剪贴板旧轮询器残留：`clipGen` 代际计数保证旧 loop 退出；`timer` 改复用。
10. 文件名净化：`sanitizeSuggestedName` 补过滤 `:*?"<>|`、控制字符、尾部点空格、保留设备名；`Start` 的手工文件名走同一净化；`Referer` 剥 `CRLF`。
11. 更新检查（新功能）：`update.go` 查 `releases/latest`，启动时后台跑、失败静默；有新版发 `update:available` 事件，前端顶部常驻横幅；`OpenReleasePage` 只允许本项目发布页。
12. 托盘退出被 beforeClose 否决：Wails Windows Frontend.Quit 会先问 OnBeforeClose，MinimizeToTray 开时托盘 退出 与 Quit binding 被转成隐藏，图标已没但进程残留。修法：App 加 quitFlag，托盘退出与 Quit 先 requestQuit 再 runtime.Quit，beforeClose 见到置位直接放行；测试 TestBeforeCloseQuitIntentLetsTrayExitThrough 锁定。
13. 多连接在无视 Range 尾端的服务器上全灭：dl.fuckingfast.co 对任何 Range 回 start-EOF 的 Content-Range，引擎要求尾端精确相等于是每块失败重试后吞成 bytes unclaimed、0 B。修法：validateRangeResponse 接受 end 大于等于所要并只读前 n 字节；fetchSegment 尾部保留 lastErr；测试 TestDownloadRangeEndIgnoredByServer 锁定。版本升到 0.2.5（app.go + wails.json）。
14. Plan refused when slots full: StartPlan only counted newly started tasks; with MaxConcurrent slots occupied it reported no waiting tasks and never launched the watcher. Fix: active+queued counts as a valid plan. Test: TestStartPlanWithFullSlotsStillRuns.
15. Record deletion leaves junk: Remove now drops .part+sidecar for unfinished tasks (finished files never auto-deleted). README delete section + App/TaskRow texts updated. Test: TestRemoveDropsUnfinishedPartials.
16. Extension empty filename: onCreated hands over stale item.filename; re-search after size wait (background.js). Reload unpacked extension to apply.
17. Probe drops GET disposition: probeRange refused to recompute SuggestedName once the URL fallback was set (probe.go). Fix: disposition-derived name always wins. Test: TestProbePrefersDispositionOverURLToken.
18. Paused-by-default + dupe rejection: Start creates paused (manager.go), same URL+dest unfinished rejected via duplicateError, apiAdd answers 200-duplicate (no browser fallback), dialog locates row via FindUnfinishedByURL. Tests: TestNewTasksRestPaused, TestDuplicateURLIsRejected.
19. Add-time probe + content dupes: Start probes unnamed adds (probeName, fail-open) for instant name/size and layer-two dedup by host+name+size (findSameContent). Exe rebuilt.
20. Probe fast-fail: hostReachable 2s dial pre-check in probeName; dead hosts skip probing. Test: TestHostReachableFastFails.
21. BringToFront for ask dialogs: Unminimise+Show+topmost flash (app.go), called from locationAsk/clipboardURL handlers. Single-window limit documented.
22. Selection bulk verbs: runSelected in App.tsx (resume/pause/remove over selected set); no backend change.
23. Selection UX: task__check checkbox in TaskRow name cell; App toolbar buttons switch all<->selected scope; runSelected reused by all three.
24. Scoped plan: StartPlanSelected + watchPlanSelected + finishPlanScoped/End (manager.go), App/plan-button scope switch. Checkbox left of name.
25. Delete path bug: partialPaths() (manager.go) resolves dir+Name like the engine; Remove/Cancel use it. Test: TestRemoveFindsPartialsByServerName.
26. Instant start feedback: startRun emits task:progress on all paths (manager.go). Test: TestStartEmitsProgressImmediately.
27. RefreshRate setting: config.go field + refreshInterval() mapping, run() wires ProgressInterval, SettingsPage select. Test: TestRefreshIntervalMapping.
28. Queued pause button: TaskRow shows pause for queued rows (backend always allowed it); space shortcut too.
29. Live settings: Limiter.SetRate (limiter.go), applySpeedLimit + pump-on-increase in SetConfig, progress delivery watermark (download.go). Tests: TestLimiterSetRateAppliesLive, TestApplySpeedLimitRetunesLive.
30. Live refresh: progressIntervalNs atomic on Downloader, livedl registry + applyRefreshInterval, SetConfig push. Tests: TestDownloaderProgressIntervalLive, TestApplyRefreshIntervalReachesLive.
31. Column drag ripple: divider moves only its two sharing columns (TaskList.tsx startColResize).
32. Drag blink: early return on no-change in startColResize onMove.
33. Drag snap-back: clamp against current widths only, never drag-start base (TaskList.tsx).
34. Directional cascade drag: boundary moves, one side grows, other side gives nearest-first (TaskList.tsx).
35. Drag runaway: always recompute from mousedown snapshot, never accumulate onto live state.
36. Hidden helpers: hide_windows/other.go (hideFile/unhideFile); wired in openPart/saveState/publish.
37. Stale UI resurrection: reload generation guard + completed-row progress guard (App.tsx subscriptions).
38. Perf batch: fillProbedName async, batch() save coalescing, pump newest-wins, memo TaskRow, single update check.
39. Resume flush knobs: SaveInterval/SaveBytes/SavePercent (download.go), ResumeFlushKb/Ms/Pct (config.go), SettingsPage rows. Fixture rule: set Progress fields BEFORE Normalize.
40. Settings tabs + flush switches + reorder (SettingsPage.tsx, styles.css).
41. Flush checkbox style (.setting__check) + number-first order.
42. Flush rows use Switch + nested number/hint (SettingsPage.tsx).
## 8. 剩下未动的（有意延后）

- ~~`Remove`/`Cancel` 在 binding 线程同步 `waitStopped(3s)`~~：已修——等删改走后台 goroutine，`Cancel` 用代际 guard 防误删（`manager.go:Cancel/Remove`）。
- ~~`moveFile` 跨卷 `tmp` 固定名~~：已修——staging 名带 pid + 计数器（`download.go:nextTmpID`）。
- ~~`state.matches` 不比 `URL`~~：已修——`FinalURL`（都有时）否则 `URL` 不一致即丢弃（`state.go:matches`）。
- 前端 `as never` 两处（`NewTaskDialog.tsx:85`、`SettingsPage.tsx:88`）：把 `StartRequest`/`Config`
  断言掉以绕开类型检查，说明 `types.ts` 里手写的类型与 Wails 生成的 `models.ts` 有出入。未修。
  **注意：本机有 Node（v24.16.0），`npx tsc --noEmit` 与 `wails build` 都能跑**，
  `wailsjs/` 绑定也已生成（含 `RemoveTasks`/`ClearFailedTasks`/`Version`）——
  改完 Go 的 binding 后直接 `wails build` 就会重新生成，不必手写类型。
- ~~扩展 `ownDownloads` 只增不删、`handOver` 先删后送的降级保真~~：已修——`onChanged` 结算即删 + 1000 上限（`trackOwn`），回退重下带原文件名。
- `shutdownMachine` 无二次确认：保留（计划下载页已有开关），动前需与你确认。

## 8. 公开注意事项（仓库是 Public）

- `extension.pem` / `extension.crx` 在工作区但已被 `.gitignore` 忽略，**绝不 `git add -f`**；`manifest.json` 无内嵌 `key`，正常。
- 提 issue 贴日志前脱敏：`vkmdownload.log`、`tasks.json`、sidecar、`probe` 输出的 URL 可能含 `?token=/session=` 签名参数。
- 不要提交真实 `tasks.json`、用户目录的 `*.vkmdownload.json`、带签名的重定向 URL。
