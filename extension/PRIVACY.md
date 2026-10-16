# Privacy Policy — VKMDownload Browser Extension

## What this extension does

VKMDownload hands browser downloads over to the VKMDownload desktop
application running on your own machine, instead of letting the browser
download them itself.

## Data the extension handles

| Data | Why | Where it goes |
|---|---|---|
| Download URL, file name, referrer | To start the download in the desktop app | `http://127.0.0.1` (your own machine only) |
| Browser cookies for the download URL (only on hosts the extension may read: `*.google.com`, `*.googleusercontent.com`; empty elsewhere) | Some hosts require a login; without the browser's cookies the desktop app looks like a stranger and gets rejected | `http://127.0.0.1` (your own machine only) |
| Browser user-agent string | Some hosts/WAFs refuse non-browser clients | `http://127.0.0.1` (your own machine only) |
| Download-result notifications | Telling you whether a handover succeeded | Shown locally, never sent anywhere |
| Own-download IDs (session storage) | So the extension does not intercept its own fallback downloads | Browser session storage, cleared when the browser exits |

## What it does NOT do

- No data is sent to any third party. All traffic stays on loopback
  (`127.0.0.1`) between the extension and your local VKMDownload app.
- No browsing history, tabs, passwords, or page contents are read. The
  extension cannot see them: it holds no `tabs`, `history`, or `<all_urls>`
  permission.
- No remote code. Every file the extension runs ships inside its own
  package (Manifest V3, no remotely hosted code).
- No analytics, no tracking, no advertising.

## Contact

Issues: https://github.com/Vick-moo/VKMDownload/issues

---

# 隐私声明 — VKMDownload 浏览器扩展（中文）

本扩展把浏览器下载转交给**本机**的 VKMDownload 桌面程序，不再让浏览器自己下载。

**经手的数据**：下载地址、文件名、来源页（发给本机 `127.0.0.1` 交接任务用）；下载网址的 Cookie（仅有读取权限的站点可读，其余为空）与浏览器 UA（部分网站要登录态，没有会被拒，同样只发给本机）；下载结果通知与自启下载 ID（只存浏览器本地，前者显示结果，后者在浏览器退出时清除）。

**不做的事**：不向任何第三方发数据；不读历史、标签页、密码和页面内容（根本没申请这些权限）；无远程代码（MV3，跑的全是包里的文件）；无统计、无追踪、无广告。

问题反馈：https://github.com/Vick-moo/VKMDownload/issues
