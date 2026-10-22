# Privacy Policy — VKMDownload Browser Extension

> This file covers the extension only. The desktop app also keeps records on disk and
> checks GitHub for updates; see the product-level [`../PRIVACY.md`](../PRIVACY.md)
> for that side. Where the two disagree, the product-level file wins.



## What this extension does

VKMDownload hands browser downloads over to the VKMDownload desktop
application running on your own machine, instead of letting the browser
download them itself.

## Data the extension handles

| Data | Why | Where it goes |
|---|---|---|
| Download URL, file name, referrer | To start the download in the desktop app | `http://127.0.0.1` (your own machine only) |
| Browser cookies for the download URL (only on hosts the extension may read: `*.google.com`, `*.googleusercontent.com`; empty elsewhere, and never for loopback or private addresses; nothing is read at all when the app's cookie switch is off) | Some hosts require a login; without the browser's cookies the desktop app looks like a stranger and gets rejected. This includes HttpOnly cookies, which a page's own scripts cannot read | `http://127.0.0.1`; the app then replays them to the download site. For unfinished downloads the app may also keep them on disk so the transfer can resume |
| Browser user-agent string | Some hosts/WAFs refuse non-browser clients | `http://127.0.0.1` (your own machine only) |
| Download-result notifications | Telling you whether a handover succeeded | Shown locally, never sent anywhere |
| Own-download IDs, plus the ids of downloads handed over but not yet cancelled (session storage) | So the extension does not intercept its own downloads, and can finish a cancellation after its service worker restarts | Browser session storage, cleared when the browser exits |
| Pairing key (local storage, `vkmPairKey`) | Proves to the desktop app that a request comes from this extension, and proves to the extension that the responder is the app itself | Browser profile storage; removed when the extension is uninstalled |

## What it does NOT do

- No data is sent to any third party. All traffic stays on loopback
  (`127.0.0.1`) between the extension and your local VKMDownload app.
- No browsing history, passwords, or page contents are read. The extension
  holds no `tabs`, `history`, or `<all_urls>` permission and injects no scripts.
  Two exceptions worth naming: clicking the context menu item hands us the URL of
  the page you clicked on (used as the download's referrer, with its query string
  stripped) and any text you had selected; and the `downloads` permission lets us
  see a download's URL, filename and referrer.
- Downloads started in an incognito window are not taken over unless the user
  has explicitly turned on incognito handover in the app's settings (off by
  default). When it is off, the extension leaves those downloads to the browser
  and keeps no record of them.
- No remote code. Every file the extension runs ships inside its own
  package (Manifest V3, no remotely hosted code).
- No analytics, no tracking, no advertising.

## Contact

Issues: https://github.com/Vick-moo/VKMDownload/issues

---

# 隐私声明 — VKMDownload 浏览器扩展（中文）

本扩展把浏览器下载转交给**本机**的 VKMDownload 桌面程序，不再让浏览器自己下载。

**经手的数据**：下载地址、文件名、来源页（发给本机 `127.0.0.1` 交接任务用）；下载网址的 Cookie（仅有读取权限的站点可读，其余为空；应用里可关掉，关掉后扩展根本不读）与浏览器 UA（部分网站要登录态，没有会被拒，同样只发给本机）；下载结果通知与自启下载 ID（只存浏览器本地，前者显示结果，后者在浏览器退出时清除）。

**不做的事**：不向任何第三方发数据；不读历史、标签页、密码和页面内容（根本没申请这些权限）；无远程代码（MV3，跑的全是包里的文件）；无统计、无追踪、无广告。

问题反馈：https://github.com/Vick-moo/VKMDownload/issues
