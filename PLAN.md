# vdownload — 实施计划

Go 编写的多连接下载管理器（IDM 风格），带 Wails 桌面 GUI。

---

## 0. 当前状态

### 已完成并验证

| 项 | 证据 |
|---|---|
| 多连接分片下载引擎 | 实测 1→16 连接：2.0 MiB/s → 25.4 MiB/s（**12.68x**），每级都做逐字节内容校验 |
| 断点续传（`.part` + `.json` sidecar） | 取消后恢复，5 个不同取消点全部 0 空洞 |
| 故障注入测试服务器 | 假 Range / 断流 / 谎报长度 / 503 / 无 HEAD / chunked / Cookie 保护 |
| CLI（`get` / `info` / `bench`） | `idm bench` 可跑出提速比与逐字节校验 |
| 测试套件 | `go test -race` 全绿（34.9s） |

### 本次接手后修掉的 bug

1. **丢数据（严重）** — `fetchSegment` 把"一个 reserve 块下完"当成"整个分片下完"。
   每个分片只留第一个 512 KiB，32 MiB 的文件只发布约 9 MiB。修法：用
   `errSegmentIncomplete` 哨兵区分"块完成"与"分片完成"。
2. **数据竞争 → 续传空洞（严重）** — `splitLargest` 在调度器 goroutine 里改
   `Segment.End`，worker 无锁读 `Segment.Remaining()`。worker 读到过期的较大剩余量
   → 误判分片完成 → 那部分字节永远没人下。race detector 直接指出两处行号。
   修法：所有 `Segment` 字段访问收进 `segmentManager` 的持锁方法。
3. **路径穿越（安全）** — `sanitizeFilename` 先把 `\` `/` 换成 `_` 再截断，于是
   `..\..\evil.exe` 被"拍平"成一个合法文件名，危险内容留在里面。修法：先取 basename
   再做字符折叠；顺带补上 Windows 保留设备名（`NUL`、`CON`…）拦截。
4. **sidecar 复活** — `publish` 先 `os.Remove(state)`，之后 `track.finish()` 又把它写回去。
   修法：调整顺序，并保留最终删除。
5. **probe 无重试** — 探测阶段的 503 会让整个下载直接失败，per-segment 退避根本没机会跑。
   修法：抽出 `doWithRetry`，HEAD 与 Range 探测都走它。
6. **`matches` 过于宽松** — 验证器"一边有一边没有"被当作相同。改为严格相等：
   误判代价不对称（丢一次重下 vs 发布一个长度正确但有洞的文件）。

---

## 1. 环境（已核实）

| 组件 | 版本 | 说明 |
|---|---|---|
| Go | 1.26.4 | |
| Wails CLI | v2.16.0 | `C:\Users\vickm\go\bin\wails.exe` |
| Node / npm | v24.16.0 / 11.13.0 | 前端构建 |
| WebView2 Runtime | 154.0.4258.62 | Wails 运行时依赖，已装 |
| gcc | msys64 mingw64 | cgo（`-race` 与新能验证需要） |

**Wails v2 内置剪贴板 API**（`runtime.ClipboardGetText` / `ClipboardSetText`），
因此剪贴板 URL 监控**不需要第三方依赖**。参考：
[Wails v2 Clipboard 文档](https://wails.io/docs/reference/runtime/clipboard/)。

---

## 2. 架构

### 2.1 关键约束：binding 不能阻塞

Wails 的 Go 方法是**同步** RPC。如果把整段下载放在 `StartTask` 里执行，前端会一直
卡在 await 上，窗口无响应。因此：

```
StartTask(req) ──立刻返回 taskID──▶ 前端拿到 id，开始监听事件
      │
      └─▶ go task.run()  ──▶ 引擎 Progress 回调
                                │
                                └─▶ wailsRuntime.EventsEmit("task:progress", {...})
                                        │
                                        └─▶ 前端 on('task:progress') 更新进度条
```

- 任务状态保存在 `Manager`（mutex 保护），前端可随时 `ListTasks()` 拉全量做对账
- 每个任务一个 `context.CancelFunc`，暂停/取消就是 cancel
- 进度事件按引擎已有的节流（250ms / 1MiB）发射，避免刷爆前端

### 2.2 任务持久化

引擎的 sidecar（`*.vdownload.json`）本身就是完整的任务描述（URL、大小、ETag、分片表）。
GUI 启动时：

1. 读 GUI 自己的任务索引（`%APPDATA%\vdownload\tasks.json`）——记录用户意向、
   下载目录、创建时间等 GUI 层面的信息；
2. 对每个未完成任务检查 sidecar 是否存在 → 决定其状态是"可续传"还是"已丢失"。

**任务索引与引擎 sidecar 分离**：索引是 GUI 的视图，sidecar 是引擎的真相。
索引损坏不影响续传，sidecar 缺失也能在 UI 上明确显示"无法续传，需重新下载"。

### 2.3 目录结构

```
vdownload/
├── go.mod
├── internal/engine/          # 下载引擎（已完成）
├── testsrv/                  # 故障注入测试服务器（已完成）
├── cmd/
│   ├── idm/                  # CLI（已完成）
│   └── testsrv/              # 独立测试服务器（已完成）
└── desktop/                  # Wails 应用（新增）
    ├── main.go               # wails.Run + 窗口配置
    ├── app.go                # 暴露给前端的 binding
    ├── manager.go            # 任务管理器：状态机、事件、取消
    ├── config.go             # 设置读写 + 逐项开关
    ├── clipboard.go          # 剪贴板 URL 监控
    └── frontend/             # React + Vite
        ├── src/
        │   ├── App.tsx
        │   ├── api.ts             # 生成的 binding 封装 + 事件订阅
        │   ├── components/
        │   │   ├── NewTaskDialog.tsx
        │   │   ├── TaskList.tsx
        │   │   ├── TaskRow.tsx     # 进度条 / 速度 / ETA / 分片数
        │   │   └── SettingsPage.tsx
        │   └── types.ts
        └── wailsjs/          # Wails 生成，不手改
```

### 2.4 前端 binding 设计

```
ListTasks()                  []TaskView
StartTask(opts)              taskID      // 立刻返回
PauseTask(id)                error
ResumeTask(id)               error
CancelTask(id)               error
RemoveTask(id, deleteFiles)  error
RevealInExplorer(id)         error       // 打开所在目录并选中
OpenFile(id)                 error       // 用默认程序打开
GetConfig() / SetConfig(cfg) Config
PickDirectory()              string      // runtime.OpenDirectoryDialog
TestClipboardURL()           string      // 便于"立即测试"按钮
```

事件：`task:added` / `task:progress` / `task:done` / `task:failed` / `clipboard:url`

---

## 3. 设置页：逐项可开关

按你的要求，每一项都能单独开关，并且**明确区分"加进列表"和"直接开始"**：

| 设置项 | 默认 | 说明 |
|---|---|---|
| **自动开始下载** | **关** | 关=新任务只入列表等你点开始；开=建好立刻下。这就是你说的"不问我直接开始" |
| 剪贴板监控 | 关 | 检测到剪贴板里的 URL 时按下面两条动作 |
| └ 监控时自动开始 | 关 | 关=只提示/入列表；开=直接下 |
| └ 监控时弹窗确认 | 开 | 开=弹出"发现链接，要下载吗？" |
| 默认下载目录 | `~/Downloads` | |
| 默认连接数 | 8 | |
| 最小分片大小 | 1 MiB | 小文件不切分 |
| 全局限速 | 不限 | |
| 代理 | 跟随系统 | |
| 完成后打开文件 | 关 | |
| 完成后打开所在目录 | 关 | |
| 同时进行的任务数 | 2 | 超出则排队 |
| 关闭窗口时 | 最小化到托盘 | 下载中不误退出 |

剪贴板实现：**轮询**（默认 500ms）`ClipboardGetText`，比对上一次的值，
只有内容变化且能解析出 http/https URL 才触发；同一 URL 去重，避免重复弹窗。
轮询而非事件，是因为 Wails v2 没有剪贴板变更事件。

---

## 4. 实施步骤

每一步都**先编译、再测试、再进行下一步**，不一次性铺开：

1. **脚手架** — `wails init -n desktop -t react-ts`，接入 `internal/engine`，
   空窗口能 `wails dev` 跑起来。验证：窗口出现且无报错。
2. **任务管理器** — `manager.go` + 状态机 + 事件发射；用假进度先跑通事件链路。
   验证：前端能收到心跳事件。
3. **新建任务 + 列表 + 进度** — 真实接引擎。验证：下真实文件，进度条走到 100%，
   文件在目标目录且大小正确。
4. **暂停 / 继续 / 取消** — 接 `context`。验证：暂停后 `.part` 留存，继续后
   **内容逐字节正确**（复用 CLI 那套校验思路，用 testsrv 做端到端）。
5. **设置页 + 持久化** — 逐项开关落盘。验证：改设置→重启→设置还在。
6. **剪贴板监控** — 轮询 + 去重 + 按开关动作。验证：复制一个 URL 能触发预期行为。
7. **打开文件 / 打开目录 / 托盘** — Wails runtime + 托盘菜单。
8. **打包** — `wails build`，产出 `desktop/build/bin/vdownload.exe`。验证：双击能跑。

---

## 5. 上传 GitHub（待确认）

需要你提供：
- GitHub 用户名 / 目标仓库名（建议 `vdownload` 或 `go-idm`）
- 公开还是私有
- 用 `gh` CLI 还是你自己建好空仓库我推上去

我会做：确认 `gh auth status` → 建仓 → 写 README/LICENSE/.gitignore → 首次提交 → 推送。
**不会**替你创建账号或输入凭据。

---

## 6. 已知的取舍与未做的事

- **不做浏览器扩展**：你选了"第一版先剪贴板监控"。扩展（Manifest V3 + native messaging）
  是独立的一大块，可以后续加。
- **不做视频嗅探**：IDM 的招牌功能之一，但那是另一个项目规模（解析 m3u8/DASH、
  合并 TS 分片）。当前引擎只做直链文件。
- **HTTP/2 被刻意禁用**：`transport.go` 里 pin 到 HTTP/1.1。这是分片下载能真正加速的
  前提——h2 会把 N 个 Range 请求复用到**一条** TCP 连接上，多连接的加速就变成纯装饰。
- **`bench/` 目前是空目录**：CLI 的 `idm bench` 已经覆盖了对比需求。若要 Go 原生
  benchmark 可以后续补，否则应删除该空目录。
- **排障日志**：`VDOWNLOAD_DEBUG=1` 打开引擎的 `debugf` 输出（默认静默）。GUI 阶段
  遇到疑难时这个开关是主要抓手。
