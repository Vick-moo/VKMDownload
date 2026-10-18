# 隐私说明 — VKMDownload

> 本文件覆盖 **桌面程序 + 浏览器扩展** 整体。扩展单独的那份（`extension/PRIVACY.md`）
> 仍然有效，但它只讲扩展经手的数据；两者不一致时以本文件为准。
> 最后更新：2026-10-19（对应 0.2.7 之后的修复批次）。

## 一句话

VKMDownload 不收集、不上传你的下载记录。它把下载信息留在你自己的电脑上，唯一的联网行为
是启动时向 GitHub 查一次有没有新版本。

## 1. 程序会存什么、存在哪

全部在 `%APPDATA%\VKMDownload\`（你的用户目录下），以及下载文件旁边。

| 文件 | 存什么 | 为什么 |
|---|---|---|
| `tasks.json` | 任务列表：下载地址、文件名、保存路径、大小、进度、状态、错误信息 | 重启后能恢复列表与进度 |
| `tasks.json` | 浏览器 Cookie、来源页、User-Agent（**仅未完成的任务**） | 续传需要完整的登录态和来源，否则会被站点拒绝 |
| `vkmdownload.log` | 运行日志：任务事件、错误、安装/更新动作 | 出问题时能定位；你自己决定是否发给别人 |
| `config.json` | 你的设置（保存目录、并发数、限速、代理、归档规则、扩展开关） | 记住设置 |
| `pair.key` | 扩展与本机的配对密钥（32 字节随机，权限 0600） | 让扩展能确认"对面的确是 VKMDownload" |
| `update-state.json` | 上次更新的阶段、新旧版本 SHA-256、文件路径 | 更新失败时能恢复旧版 |
| 下载文件旁的 `*.vkmdownload.json` | 断点续传记录：下载地址（**含签名参数**）、已下载范围、文件路径 | 暂停/失败后能接着下 |

**关于签名链接**：很多下载地址把凭据放在查询串里（`token=`、`sig=`、`jwt=`、
`X-Amz-Signature=`、Azure 的 `se`/`skt` 等）。这类地址**必然**存在于 `tasks.json`、
sidecar 和日志里——没有它就没法续传。

**已经做的收敛**：

- 任务**完成**后，`tasks.json` 里的下载地址会去掉查询串，Cookie / 来源页 / User-Agent
  直接清空（这些信息对已完成的任务已经没有用处）。
- 日志里所有 http(s) 地址只保留"主机 + 路径"，查询串与片段不落盘；User-Agent 只记长度。
- 启动时会一次性清洗旧版本写下的记录与日志（去查询串、清已完成任务的凭据）。
- 已完成/已取消/已失败的任务记录保留 **90 天**后自动清理；未完成的任务不清理。

## 2. 程序会往外发什么

| 场景 | 发到哪 | 发什么 |
|---|---|---|
| 下载文件 | 你给的下载地址 | 该站点需要的 Cookie、来源页、User-Agent（这是下载本身） |
| 检查更新 | `api.github.com` | 请求本身会暴露你的 IP 与访问时间；请求头只有 `Accept` 和 `VKMDownload/<版本>`。**不含**安装 ID、机器标识、你的任务或下载内容 |
| 打开"更新内容" | `api.github.com` | 同上，另外带一个版本号 |
| 下载新版本 | GitHub 的 release 下载地址 | 同上 |

没有任何统计、埋点、崩溃上报或广告 SDK。程序不会把你的下载地址、Cookie 或文件名发给
本项目的开发者。

**扩展**只与本机 `127.0.0.1` 上的这个程序通信，不发往任何第三方。

## 3. 扩展会经手什么

- 下载地址、文件名、来源页、文件大小、浏览器 UA → 只发给本机 `127.0.0.1`。
- 该下载地址对应的浏览器 Cookie（含 HttpOnly、父域 `.google.com` 的账户级 Cookie）→
  只发给本机程序。仅当扩展拥有该站点权限时才能读到（当前是 `*.google.com`、
  `*.googleusercontent.com`）；本机/内网地址**不读**。
- 来源页只取"协议 + 主机 + 路径"，**不含查询串**（避免把搜索词、文档 ID 带进任务记录）。
- **隐身模式下不接管**，下载仍由浏览器自己完成。

扩展**不申请** `tabs`、`history`、`<all_urls>`，不注入页面脚本，不读页面内容、历史或密码。
右键菜单点"用 VKMDownload 下载"时，扩展会收到你点击时所在的页面地址与你选中的文字——
前者只用作来源页（已去掉查询串），后者只用来挑出其中的链接。

## 4. 配对密钥是什么

让扩展确认"回答我的是真的 VKMDownload"，也让你确认"把下载交给的是真的扩展"。

- 密钥每次安装生成一次，存 `pair.key`（程序侧）与浏览器扩展存储（扩展侧），不写日志。
- 首次配对只有在对方**是被签名校验认可的浏览器进程**时才会给出密钥；
  之后每次交接前，扩展都会要求对方现场证明它持有这个密钥。
- 已配对之后，密钥**不会被静默替换**：如果程序重装或 `pair.key` 被删除，扩展会显示
  "没有运行"，需要你在扩展弹窗里点「重新配对」。
- 想彻底解除配对：删除 `pair.key` 并在扩展弹窗点「重新配对」。

## 5. 你想清掉数据时

- **单条**：在列表里删除任务。已完成任务的文件会保留（除非你勾选同时删除文件）；
  未完成任务的半成品与续传记录一起删除。
- **全部记录与日志**：程序提供了清除能力，会删掉 `tasks.json`、日志、以及旧版本留下的
  记录副本。**下载好的文件、你的设置、配对密钥不会被删。**
- **卸载扩展**：浏览器会清掉扩展存储（含配对密钥）。程序侧的 `pair.key` 与任务记录仍在，
  需要按上面两步自行清理。

## 6. 已知的取舍（明确告诉你，而不是藏着）

1. **未完成的任务会把 Cookie 明文存在 `tasks.json` 里**。续传必须能原样重放登录态，而
   加密需要额外的密钥管理；当前版本选择明文 + 用户目录权限。如果你会长期暂停一个需要
   登录的下载，建议完成后删除该任务，或直接取消。
2. **断点续传记录（sidecar）里有带签名的下载地址**，它就在下载目录里。文件下载完成后
   记录会被删除；暂停或失败时会保留，删除任务时一并删除。
3. **日志不会脱敏文件名与本机路径**（只脱 URL 查询串与 User-Agent）。把日志发给别人前
   请自己过一眼。
4. **更新检查没有关闭开关**，每次启动都会访问一次 GitHub。
5. 程序不做"云同步"；如果你把 `%APPDATA%` 或下载目录放进 OneDrive 之类的同步盘，
   上面这些文件会跟着被同步——那是同步工具的行为，不是本程序的。

## 7. 联系方式

问题反馈：https://github.com/Vick-moo/VKMDownload/issues

---

## English summary

VKMDownload keeps your download records on your own computer. It sends nothing to the
vendor: no analytics, no telemetry, no crash reporting. The only outbound request beyond
your downloads is a startup check of `api.github.com` for a newer release, which sends your
IP address, the time, and `VKMDownload/<version>` as a user agent — no install ID, no machine
ID, no task data.

Download URLs, cookies (only for unfinished tasks), and the local paths involved are stored
under `%APPDATA%\VKMDownload\` and next to each partial download. Signed query strings are
removed from `tasks.json` once a task completes, and URLs in the log are reduced to
host + path. The browser extension talks only to `127.0.0.1` and never to a third party.

See sections 1–6 for exactly what is kept, why, and how to remove it.
