# VDownload

一个用 Go 写的多连接下载管理器（IDM 风格），带 Wails 桌面界面。

把一个文件切成多段并行下载，支持断点续传；引擎自带一个"恶意服务器"测试工具，
用来验证它在真实网络里遇到的各种坑（服务器谎报支持 Range、中途断线、谎报文件大小……）
时不会悄悄给你一个坏文件。

> **状态**：引擎与 CLI 已完成并通过验证。桌面 GUI 正在开发中。

---

## 实测性能

对本地测试服务器（32 MiB，每连接限速 2 MiB/s）实测，每一级都做了**逐字节内容校验**：

| 并发连接 | 耗时 | 吞吐 | 相对单连接 |
|---:|---:|---:|---:|
| 1 | 16.0s | 2.0 MiB/s | 1.00x |
| 2 | 8.0s | 4.0 MiB/s | 2.00x |
| 4 | 4.0s | 8.0 MiB/s | 3.99x |
| 8 | 2.1s | 15.0 MiB/s | 7.50x |
| 16 | 1.3s | 25.4 MiB/s | **12.68x** |

为什么要在本地做限速测试：loopback 上 RTT 几乎为零，不限速的话单连接就能跑满，
多连接的优势完全看不出来。限速后才复现了真实互联网上"多连接绕过单连接限速"的场景。

---

## 功能

- **多连接分片下载** —— 每段一个独立 HTTP 请求，各自写入文件的不同偏移
- **动态分段** —— 某个 worker 空闲时，把最长的剩余段对半切开交给它，不让慢连接拖住整体
- **断点续传** —— 进度写进 sidecar（`.vdownload.json`），取消 / 崩溃后可继续
- **一致性校验** —— 用 ETag / Last-Modified 判断服务器端文件是否变过；变了就重下，不拼接
- **自适应降级** —— 服务器不支持 Range 时自动退回单连接流式下载
- **重试与退避** —— 网络中断、5xx、429 按指数退避重试；4xx 直接失败不做无用功
- **故障注入测试服务器** —— 可模拟断线、假 Range、谎报 Content-Length、403、chunked 等

---

## 快速开始

需要 Go 1.24+。

```bash
go build -o bin/vdl.exe ./cmd/idm
```

### 下载一个文件

```bash
vdl get https://example.com/big.zip -o D:\Downloads -conns 8
```

### 先看看服务器能力再决定

```bash
vdl info https://example.com/big.zip
```

输出会告诉你文件多大、**实测**是否真的支持 Range（不信服务器自称的 `Accept-Ranges`）、
以及建议的文件名。

### 对比不同连接数的速度

```bash
vdl bench https://example.com/big.zip -conn-list 1,2,4,8,16
```

### 断点续传

中途 `Ctrl-C` 会保留 `.part` 和 `.json`，下次带 `-resume` 继续：

```bash
vdl get https://example.com/big.zip -o D:\Downloads -resume
```

### 常用参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-conns` | 8 | 并发连接数，1 表示禁用分段 |
| `-o` | 服务器建议的文件名 | 输出文件或目录 |
| `-min-segment` | 1 MiB | 最小分片大小，小文件不会被切碎 |
| `-reserve` | 512 KiB | worker 一次认领多少字节 |
| `-H` | | 附加请求头，可重复，如 `-H 'Cookie: a=b'` |
| `-proxy` | | 代理地址，留空则读环境变量 |
| `-json` | | 以 JSON 输出结果 |

---

## 测试

```bash
go test ./...            # 全部测试
go test -race ./...      # 并发正确性（推荐，历史上抓出过真 bug）
VDOWNLOAD_DIAGNOSE=1 go test ./internal/engine -run Diagnose -v   # 长时间压力诊断
```

排障时打开引擎的详细日志：

```bash
VDOWNLOAD_DEBUG=1 vdl get <url>          # 打到 stderr
VDOWNLOAD_DEBUG=C:\trace.log vdl get <url>   # 打到文件
```

---

## 设计要点

### HTTP/2 是被**故意**关掉的

`internal/engine/transport.go` 把连接固定到 HTTP/1.1（`TLSNextProto` 设为空 map）。
这不是风格选择，而是分片下载能加速的**前提**：HTTP/2 会把同一域名的所有请求
复用到**一条** TCP 连接上，于是 16 个 Range 请求挤在同一条流里，一起撞上那条流的
拥塞窗口——看起来并行，实际等于没并行；更糟的是，按连接限速的服务器会把 16 个
"并行"流一起限速。HTTP/1.1 下一条连接只承载一个进行中的请求，N 个并发 Range
请求必然是 N 条独立 TCP 连接。

### 磁盘上的三件套

```
big.zip                    成品
big.zip.vdownload.part     半成品数据（各 worker 用 WriteAt 写入自己的偏移）
big.zip.vdownload.json     分段进度 + ETag + 总大小
```

全部下载完成后，`.part` 被重命名为成品名，`.json` 被删除。
续传前会校验 ETag / Last-Modified / 总大小，任何一项不一致就丢弃重来——
把两次不同版本的数据拼在一起，会得到一个长度完全正确、内容却是坏的的文件。

### 分片进度用两个计数器，不是一个

这是踩过坑的地方，值得单独说。

一开始只用一个计数器，它同时表示"已预留"和"已写入"。多个 worker 会对它做
`+= n`（认领）和 `-= n`（失败退回），这些加减**交错**之后，最终值取决于执行顺序，
而不再等于真实落盘的字节数。一旦偏高，下一个请求的偏移量就会跳到没人下载过的位置，
那一段就永远被跳过——而**文件长度依然是正确的**，任何长度校验都抓不到。

现在拆成两条独立轨道：

- `Reserved` 只用来防止两个 worker 抢同一段；
- `Written` **只**在数据真正 `WriteAt` 落盘之后才推进，并且是唯一决定"下一个请求位置"的量。

这样无论并发如何交错，请求偏移量都只反映真实写进磁盘的字节，物理上不可能跳过空洞。

### 测试服务器比真服务器更"坏"

`testsrv/` 是一个可编程的恶劣服务器：`-fake-range` 会自称支持 Range 却总是返回 200；
`-drop-after` 在指定字节后硬断连接；`-lie-content-length` 谎报长度；
`-no-head` 对 HEAD 返回 405；`-require-cookie` 不给 cookie 就 403。
引擎的探测逻辑之所以**不信** `Accept-Ranges` 头、只信实测的 206 响应，就是被它逼出来的。

---

## 目录结构

```
internal/engine/     下载引擎（探测、分段规划、并发下载、续传、重试）
testsrv/             故障注入测试服务器（可被测试 import）
cmd/idm/             命令行入口
cmd/testsrv/         独立运行的测试服务器
desktop/             Wails 桌面应用（开发中）
PLAN.md              实施计划与已知取舍
```

---

## 已知边界

- **不做浏览器扩展**：目前靠剪贴板监控捕获链接，浏览器集成尚未实现
- **不做视频嗅探**：IDM 的招牌功能之一，但那是另一个项目规模（m3u8/DASH 解析 + TS 合并）
- 引擎只处理直链文件下载

---

## 许可证

MIT，见 [LICENSE](LICENSE)。
