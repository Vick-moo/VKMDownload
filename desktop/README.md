# VKMDownload 桌面端

Wails v2 + React 19 的桌面界面，壳层在父目录的下载引擎之上。

## 开发

```powershell
wails dev
```

会起一个 Vite 开发服务器（热更新），同时打开应用窗口。
`http://localhost:34115` 是带 Go 运行时注入的调试地址，可以用浏览器 devtools 直接调 Go 方法。

## 构建

```powershell
wails build
```

产物：`build/bin/VKMDownload.exe`。

## 结构

| 路径 | 作用 |
|---|---|
| `main.go` | 窗口配置（尺寸、最小尺寸、深色标题栏、生命周期钩子） |
| `app.go` | 暴露给前端的 binding |
| `api.go` | 浏览器扩展用的回环 HTTP 端点（`/ping`、`/settings`、`/add`） |
| `manager.go` | 任务管理：调度、暂停/继续、批量操作、计划下载、持久化 |
| `config.go` | 设置结构与默认值 |
| `frontend/src/App.tsx` | 界面骨架：侧栏筛选、工具栏、列表区 |
| `frontend/src/components/Icons.tsx` | 内联 SVG 图标（不引图标库） |
| `frontend/src/styles.css` | 设计 token 与全部组件样式 |
| `frontend/src/types.ts` | 与 Go 结构体对应的类型（Go 侧改了要同步改这里） |
| `../../design/DESIGN.md` | 界面设计规范，改界面前先读 |
| `../../extension/` | 配套的浏览器扩展 |

## 依赖引擎的方式

`go.mod` 里用 `replace vkmdownload => ../` 指向父目录，所以本地构建用的**始终是工作区的引擎代码**，
而不是某个已发布版本。改了引擎不必发版，重新 `wails build` 即可。

## 设置存放位置

`%APPDATA%\VKMDownload\`：

| 文件 | 内容 |
|---|---|
| `config.json` | 全部设置 |
| `tasks.json` | 任务列表（重启后恢复） |
| `vkmdownload.log` | 诊断日志，含每个经由扩展创建的下载及其来源 |

设置统一存放在 `%APPDATA%\VKMDownload\`，全新安装直接使用该目录，不做旧目录迁移。

## 注意

- `wailsjs/` 是 Wails 生成的绑定，已加入忽略列表，不要手改。
- `frontend/package.json.md5` 是 Wails 用来判断依赖是否需要重装的本地缓存，同样不提交。

