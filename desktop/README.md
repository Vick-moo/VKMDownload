# VDownload 桌面端

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

产物：`build/bin/VDownload.exe`。

## 结构

| 路径 | 作用 |
|---|---|
| `main.go` | 窗口配置（尺寸、最小尺寸、深色标题栏、生命周期钩子） |
| `app.go` | 暴露给前端的 binding |
| `frontend/src/App.tsx` | 界面骨架：顶栏、侧栏筛选、工具栏、列表区 |
| `frontend/src/components/Icons.tsx` | 内联 SVG 图标（不引图标库） |
| `frontend/src/styles.css` | 设计 token 与全部组件样式 |
| `frontend/src/types.ts` | 与 Go 结构体对应的类型（Go 侧改了要同步改这里） |
| `../../design/DESIGN.md` | 界面设计规范，改界面前先读 |

## 依赖引擎的方式

`go.mod` 里用 `replace vdownload => ../` 指向父目录，所以本地构建用的**始终是工作区的引擎代码**，
而不是某个已发布版本。改了引擎不必发版，重新 `wails build` 即可。

## 注意

- `wailsjs/` 是 Wails 生成的绑定，已加入忽略列表，不要手改。
- `frontend/package.json.md5` 是 Wails 用来判断依赖是否需要重装的本地缓存，同样不提交。
