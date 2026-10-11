/**
 * VKMDownload's browser extension.
 *
 * It does two things: take over a download the browser is about to start, and
 * offer the same on the context menu. Both end up at the app's loopback
 * endpoint, which creates the task.
 *
 * There is no native messaging host. That is the approach IDM uses and it is
 * more invasive than it needs to be: one helper binary plus a registry key per
 * browser, each of which can fail on its own. The app already runs; it exposes
 * a loopback port, and the extension finds it.
 *
 * This is a service worker, so it can be stopped at any time and must not rely
 * on anything it happens to be holding in memory.
 */

const PORT_FIRST = 18888;
const PORT_LAST = 18898;

/**
 * The base URL of the running app, once found.
 *
 * Cached in the worker's memory for as long as it lives, and cleared whenever a
 * request fails — the app may have been closed and restarted on another port.
 */
let apiBase = null;

/**
 * Downloads this extension started itself, so it does not intercept them again.
 *
 * Without this the "take over" path and the "hand it back" path would feed each
 * other: the app is asked to fetch, the browser sees a download, and the
 * extension tries to take that over too.
 */
const ownDownloads = new Set();

// --------------------------------------------------------------- talking ---

/** Finds the app, trying each port it may be listening on. */
async function findApp() {
  if (apiBase) return apiBase;

  for (let port = PORT_FIRST; port <= PORT_LAST; port += 1) {
    const candidate = `http://127.0.0.1:${port}`;
    try {
      const response = await fetch(`${candidate}/ping`, {
        cache: "no-store",
        signal: AbortSignal.timeout(600),
      });
      if (!response.ok) continue;

      const body = await response.json();
      // Another program could hold the port; only the app identifies itself.
      if (body && body.app === "VKMDownload") {
        apiBase = candidate;
        return apiBase;
      }
    } catch {
      // Nothing there, or it did not answer in time: try the next port.
    }
  }
  return null;
}

/** Hands one download to the app. */
async function handOver(payload) {
  const base = await findApp();
  if (!base) {
    return { ok: false, error: "VKMDownload 没有运行" };
  }

  try {
    const response = await fetch(`${base}/add`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    const body = await response.json().catch(() => ({}));

    if (!response.ok) {
      return { ok: false, error: body.error || `HTTP ${response.status}` };
    }
    return { ok: true, name: body.name || "" };
  } catch (error) {
    // The app may have exited between the ping and this call.
    apiBase = null;
    return { ok: false, error: String(error) };
  }
}

/** Tells the user what happened, since neither path has a UI of its own. */
function report(result, fallbackName) {
  const succeeded = result.ok;
  chrome.notifications.create({
    type: "basic",
    iconUrl: "icons/icon128.png",
    title: succeeded ? "已交给 VKMDownload" : "VKMDownload 没有响应",
    message: succeeded
      ? fallbackName || result.name || "已加入下载队列"
      : `${result.error} · 请先启动 VKMDownload`,
    priority: succeeded ? 0 : 2,
  });
}

/** The last path segment of a URL, decoded, for display only. */
function nameFromUrl(raw) {
  try {
    const url = new URL(raw);
    const last = url.pathname.split("/").filter(Boolean).pop() || "";
    return decodeURIComponent(last);
  } catch {
    return "";
  }
}

// ---------------------------------------------------------------- policy ---

/**
 * The interception policy, cached for a moment.
 *
 * The switches live in the app's settings, so this asks rather than deciding
 * for itself: one place to change them, and a change applies to the very next
 * download with no reinstall. The cache keeps that from costing a round trip on
 * every download, and is short enough that an edit feels immediate.
 */
const POLICY_TTL_MS = 3000;
let policyCache = { at: 0, value: null };

async function getPolicy() {
  const now = Date.now();
  if (policyCache.value && now - policyCache.at < POLICY_TTL_MS) {
    return policyCache.value;
  }

  const base = await findApp();
  if (!base) {
    policyCache = { at: 0, value: null };
    return null;
  }

  try {
    const response = await fetch(`${base}/settings`, {
      cache: "no-store",
      signal: AbortSignal.timeout(1200),
    });
    const value = await response.json();
    policyCache = { at: now, value };
    return value;
  } catch {
    apiBase = null;
    policyCache = { at: 0, value: null };
    return null;
  }
}

/** The host of a URL, without the www. prefix, lower-cased. */
function hostOf(raw) {
  try {
    return new URL(raw).hostname.toLowerCase().replace(/^www\./, "");
  } catch {
    return "";
  }
}

/** Whether a host, or any parent of it, is on the skip list. */
function hostSkipped(host, list) {
  if (!host || !Array.isArray(list)) return false;
  return list.some((entry) => host === entry || host.endsWith(`.${entry}`));
}

/**
 * The download's size in KiB, or -1 when it is not known.
 *
 * Chrome usually does not know the length at onCreated — it learns it from the
 * response headers a moment later — so this waits briefly and asks again. The
 * alternative is sending our own HEAD request, which needs a host permission
 * for every site the user might download from; a 250ms wait and no extra
 * permission is the better trade.
 */
async function sizeKb(item) {
  if (item.totalBytes > 0) return item.totalBytes / 1024;

  await new Promise((resolve) => setTimeout(resolve, 250));
  try {
    const [found] = await chrome.downloads.search({ id: item.id });
    if (found && found.totalBytes > 0) return found.totalBytes / 1024;
  } catch {
    // The item may already be gone.
  }
  return -1;
}

// ------------------------------------------------------------ intercepting ---

/** Whether a download is one this extension should take over. */
function shouldTake(item) {
  const url = item.finalUrl || item.url || "";

  // Only real network downloads. A blob: or data: URL has no address the app
  // could fetch, and file: is the user opening something local.
  if (!/^https?:/i.test(url)) return false;

  // Anything the extension itself started.
  if (ownDownloads.has(item.id)) return false;

  return true;
}

chrome.downloads.onCreated.addListener(async (item) => {
  if (!shouldTake(item)) return;

  // Ask the app what it wants. When it is not running this returns null, and
  // the download is left to the browser: taking it over and then failing would
  // lose it entirely, which is far worse than not interfering.
  const policy = await getPolicy();
  if (!policy || !policy.enabled) return;

  const url = item.finalUrl || item.url;

  if (hostSkipped(hostOf(url), policy.skipHosts)) return;

  // Small files are not worth a separate program, and taking them over is what
  // makes an interception extension annoying: browser updates, favicons and the
  // images in a PDF viewer are all downloads too. An unknown size is taken,
  // since the filter is a convenience and not a promise.
  const kb = await sizeKb(item);
  if (policy.minSizeKb > 0 && kb >= 0 && kb < policy.minSizeKb) return;

  // Cancel first, then hand over: the user sees the browser's entry disappear
  // rather than sit there failed next to the app's own progress.
  chrome.downloads.cancel(item.id, () => {
    void chrome.runtime.lastError;
    chrome.downloads.erase({ id: item.id }, () => void chrome.runtime.lastError);
  });

  const result = await handOver({
    url,
    filename: item.filename ? item.filename.split(/[\\/]/).pop() : "",
    referer: item.referrer || "",
  });

  // The app answered the policy question but refused the task itself. Put the
  // download back rather than silently dropping what the user asked for.
  if (!result.ok) {
    const id = await chrome.downloads.download({ url });
    if (typeof id === "number") ownDownloads.add(id);
    report(result, item.filename);
  }
});

// ---------------------------------------------------------- context menu ---

chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: "vkm-add",
      title: "用 VKMDownload 下载",
      contexts: ["link", "video", "audio", "image", "selection"],
    });
  });
});

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  if (info.menuItemId !== "vkm-add") return;

  const url = info.linkUrl || info.srcUrl || info.selectionText || "";
  if (!/^https?:/i.test(url)) {
    report({ ok: false, error: "这个地址不能下载" }, "");
    return;
  }

  const result = await handOver({
    url,
    filename: nameFromUrl(url),
    referer: tab && tab.url ? tab.url : "",
  });
  report(result, nameFromUrl(url));
});

// ------------------------------------------------------------ messages ----

/**
 * The popup asks for the policy and flips the switch.
 *
 * Both come from the app, not from extension storage: the switches live in the
 * app's settings so there is one place to change them, and the popup is a
 * shortcut to the same thing rather than a second source of truth that could
 * disagree with it.
 *
 * Answering asynchronously matters: returning true keeps the message channel
 * open until sendResponse is called, and without it the popup would always be
 * told the app is missing.
 */
chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
  if (message === "status") {
    (async () => {
      const policy = await getPolicy();
      sendResponse({
        running: Boolean(policy),
        enabled: policy ? Boolean(policy.enabled) : false,
        minSizeKb: policy ? policy.minSizeKb : 0,
      });
    })();
    return true;
  }

  if (message === "toggle") {
    (async () => {
      const base = await findApp();
      if (!base) {
        sendResponse(null);
        return;
      }

      const current = await getPolicy();
      const enabled = !(current && current.enabled);

      try {
        const response = await fetch(`${base}/settings`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ enabled }),
        });
        if (!response.ok) {
          sendResponse(null);
          return;
        }
        // The cache still holds the old value and would answer the next
        // download from it, so drop it rather than wait out its lifetime.
        policyCache = { at: 0, value: null };
        sendResponse({ enabled });
      } catch {
        apiBase = null;
        sendResponse(null);
      }
    })();
    return true;
  }

  return false;
});
