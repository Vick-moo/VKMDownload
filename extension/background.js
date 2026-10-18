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
 * Header the app uses to answer a /ping challenge.
 *
 * The app returns HMAC-SHA256(key, challenge) here. Only a holder of the
 * per-install key can produce it, which is what distinguishes the real app from
 * any other program that happens to answer on one of these ports — the body's
 * self-reported "app" field proves nothing.
 */
const PROOF_HEADER = "X-VKMDownload-Proof";

/**
 * Where the pairing key lives: chrome.storage.local, per profile.
 *
 * It is a capability over this browser's cookies and download URLs, not a
 * secret identity, so it never leaves the profile and is never logged.
 */
const KEY_STORAGE = "vkmPairKey";

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

/**
 * Whether the extension APIs this worker needs are present.
 *
 * False under node, where the test suite imports this file for its pure
 * decision logic: every listener registration below is guarded by this, so
 * importing has no side effects there.
 */
function hasExtensionApis() {
  return (
    typeof chrome !== "undefined" &&
    !!(chrome.downloads && chrome.downloads.onCreated)
  );
}

/**
 * The session-storage mirror of ownDownloads.
 *
 * The worker can be stopped at any time and lose the in-memory set above, so
 * the ids are also kept in chrome.storage.session: it survives a worker
 * restart but is cleared when the browser itself exits — exactly the lifetime
 * of a download id. Access is best-effort; when it fails the in-memory set is
 * still correct for this worker lifetime.
 */
const OWN_KEY = "ownDownloads";
let ownLoaded = false;

async function loadOwn() {
  if (ownLoaded) return;
  ownLoaded = true;
  try {
    if (
      typeof chrome !== "undefined" &&
      chrome.storage &&
      chrome.storage.session
    ) {
      const got = await chrome.storage.session.get(OWN_KEY);
      const arr = got && got[OWN_KEY];
      if (Array.isArray(arr)) {
        for (const id of arr) {
          if (typeof id === "number") ownDownloads.add(id);
        }
      }
    }
  } catch {
    // Storage unavailable: the in-memory set still covers this lifetime.
  }
}

function saveOwn() {
  try {
    if (
      typeof chrome !== "undefined" &&
      chrome.storage &&
      chrome.storage.session
    ) {
      void chrome.storage.session.set({
        [OWN_KEY]: [...ownDownloads].slice(-1000),
      });
    }
  } catch {
    // Best-effort only; see above.
  }
}

/**
 * Remembers a download the extension started itself, so its completion is not
 * mistaken for a new user download. Entries leave when the download settles;
 * the cap bounds a long-running browser profile that never restarts.
 */
function trackOwn(id) {
  if (typeof id !== "number") return;
  ownDownloads.add(id);
  if (ownDownloads.size > 1000) {
    const oldest = ownDownloads.values().next();
    if (!oldest.done) ownDownloads.delete(oldest.value);
  }
  saveOwn();
}

if (hasExtensionApis()) {
  chrome.downloads.onChanged.addListener((delta) => {
    if (!delta || typeof delta.id !== "number") return;
    const state = delta.state && delta.state.current;
    if (state === "complete" || state === "interrupted") {
      ownDownloads.delete(delta.id);
      saveOwn();
    }
  });
}

// --------------------------------------------------------------- talking ---

/** Cached pairing key, loaded once per worker lifetime. */
let pairKey = null;

/** Reads the stored key; an unreadable store means "not paired yet". */
async function loadPairKey() {
  if (pairKey) return pairKey;
  try {
    if (typeof chrome !== "undefined" && chrome.storage && chrome.storage.local) {
      const got = await chrome.storage.local.get(KEY_STORAGE);
      const k = got && got[KEY_STORAGE];
      if (typeof k === "string" && /^[0-9a-f]{64}$/i.test(k)) pairKey = k;
    }
  } catch {
    // No storage: pair again below rather than failing the download.
  }
  return pairKey;
}

async function savePairKey(key) {
  pairKey = key;
  try {
    if (typeof chrome !== "undefined" && chrome.storage && chrome.storage.local) {
      await chrome.storage.local.set({ [KEY_STORAGE]: key });
    }
  } catch {
    // In-memory only; the next worker start pairs again.
  }
}

/** A fresh nonce per probe, so an observed proof cannot be replayed. */
function randomChallenge() {
  const raw = new Uint8Array(16);
  crypto.getRandomValues(raw);
  return [...raw].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** HMAC-SHA256(key, message) as lower-case hex. */
async function hmacHex(keyHex, message) {
  const keyBytes = new Uint8Array(keyHex.match(/../g).map((h) => parseInt(h, 16)));
  const key = await crypto.subtle.importKey(
    "raw",
    keyBytes,
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const sig = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(message));
  return [...new Uint8Array(sig)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** Constant-time-ish comparison of two equal-length hex strings. */
function proofMatches(got, want) {
  if (typeof got !== "string" || got.length !== want.length) return false;
  let diff = 0;
  for (let i = 0; i < got.length; i += 1) {
    diff |= got.charCodeAt(i) ^ want.charCodeAt(i);
  }
  return diff === 0;
}

/**
 * Asks one candidate port to prove it holds the key.
 *
 * Returns false for anything that does not answer correctly, including a port
 * held by a different program that imitates the ping body.
 */
async function provePort(base, key) {
  const challenge = randomChallenge();
  try {
    const response = await fetch(`${base}/ping?challenge=${challenge}`, {
      cache: "no-store",
      signal: AbortSignal.timeout(600),
    });
    if (!response.ok) return false;
    const got = response.headers.get(PROOF_HEADER);
    if (!got) return false;
    return proofMatches(got.toLowerCase(), await hmacHex(key, challenge));
  } catch {
    return false;
  }
}

/**
 * Asks the app for the pairing key.
 *
 * This is a GET so no preflight is needed. A refusal (403) means the caller was
 * not recognised as a signed browser, and the extension must treat that exactly
 * like "the app is not running": nothing is sent to a responder that has not
 * proven itself.
 */
async function pairWith(base) {
  try {
    const response = await fetch(`${base}/pair`, {
      cache: "no-store",
      signal: AbortSignal.timeout(800),
    });
    if (!response.ok) return "";
    const body = await response.json().catch(() => ({}));
    const key = body && body.key;
    return typeof key === "string" && /^[0-9a-f]{64}$/i.test(key) ? key : "";
  } catch {
    return "";
  }
}

/**
 * Finds the app and proves it is really VKMDownload.
 *
 * Every candidate port is probed at once rather than one after another: the
 * sequential version spent up to 11 x 600 ms before concluding the app was not
 * running, which the user felt as a pause on every download while the browser
 * carried on fetching.
 *
 * The port is only cached after a proof. A responder that answers the ping body
 * but cannot produce the HMAC is treated as an impostor and skipped, never
 * handed a URL or a cookie.
 */
async function findApp() {
  if (apiBase) return apiBase;

  const ports = [];
  for (let port = PORT_FIRST; port <= PORT_LAST; port += 1) ports.push(port);

  const candidates = (
    await Promise.all(
      ports.map(async (port) => {
        const candidate = `http://127.0.0.1:${port}`;
        try {
          const response = await fetch(`${candidate}/ping`, {
            cache: "no-store",
            signal: AbortSignal.timeout(600),
          });
          if (!response.ok) return null;
          const body = await response.json().catch(() => ({}));
          return body && body.app === "VKMDownload" ? candidate : null;
        } catch {
          return null;
        }
      }),
    )
  ).filter(Boolean);

  if (candidates.length === 0) return null;

  let key = await loadPairKey();
  for (const base of candidates) {
    if (!key) {
      const got = await pairWith(base);
      if (got) {
        await savePairKey(got);
        key = got;
      }
    }
    if (key && (await provePort(base, key))) {
      apiBase = base;
      return apiBase;
    }
  }

  // Nothing proved itself. Do not cache anything: the real app may start later,
  // and the stored key is still valid for it.
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

    // The app's peer check can refuse even a well-formed handover (unknown
    // caller): surface its reason verbatim instead of the generic
    // "not running" line.
    if (response.status === 403) {
      return { ok: false, refused: true, error: body.error || "HTTP 403" };
    }
    if (!response.ok) {
      return { ok: false, error: body.error || `HTTP ${response.status}` };
    }
    // Ask-location mode: the app holds the arrival for the user to place it,
    // so the browser entry must already be gone and must NOT come back.
    // This is handed over, not finished — never a fallback download.
    if (body.pending) {
      return { ok: true, pending: true, name: body.name || "" };
    }
    // A duplicate URL is answered 200 with the existing record: the file is
    // already coming down, so there must be no browser fallback download.
    if (body.duplicate) {
      return { ok: true, duplicate: true, name: body.name || "" };
    }
    return { ok: true, name: body.name || "" };
  } catch (error) {
    // The app may have exited between the ping and this call.
    apiBase = null;
    return { ok: false, error: String(error) };
  }
}

/**
 * Strips URLs down to host+path and removes control characters.
 *
 * Notification text is built from server- and page-supplied strings: a URL with
 * a signed query in it would put a credential on the lock screen, and a newline
 * would break the layout.
 */
function sanitizeText(raw, max) {
  let out = String(raw == null ? "" : raw);
  out = out.replace(/https?:\/\/[^\s"'<>]+/gi, (m) => {
    try {
      const u = new URL(m);
      return u.host + u.pathname;
    } catch {
      return "(link)";
    }
  });
  out = out.replace(/[\u0000-\u001f\u007f]/g, " ").trim();
  if (max && out.length > max) out = `${out.slice(0, max)}…`;
  return out;
}

/** Tells the user what happened, since neither path has a UI of its own. */
function report(result, fallbackName) {
  if (result.pending) {
    chrome.notifications.create({
      type: "basic",
      iconUrl: "icons/icon128.png",
      title: "已转到应用等确认",
      message: result.name || fallbackName || "已转到应用，等你在弹窗里确认位置",
      priority: 0,
    });
    return;
  }
  if (result.duplicate) {
    chrome.notifications.create({
      type: "basic",
      iconUrl: "icons/icon128.png",
      title: "已在下载列表中",
      message: result.name || fallbackName || "该链接已在列表中，无需重复添加",
      priority: 0,
    });
    return;
  }
  const succeeded = result.ok;
  chrome.notifications.create({
    type: "basic",
    iconUrl: "icons/icon128.png",
    title: succeeded ? "已交给 VKMDownload" : "VKMDownload 没有响应",
    message: succeeded
      ? fallbackName || result.name || "已加入下载队列"
      : result.refused
        ? result.error
        : `${result.error} · 请先启动 VKMDownload`,
    priority: succeeded ? 0 : 2,
  });
}

/**
 * The browser's cookies for a URL, serialised as a Cookie header value.
 *
 * The app downloads with its own HTTP client, so without this it looks like a
 * stranger to any host that requires a login (Google Classroom / Drive
 * private files redirect it to accounts.google.com). Returns "" when the
 * cookies cannot be read — missing host permission or an unparsable URL —
 * and the handover still proceeds without them.
 */
async function cookiesFor(raw) {
  try {
    const list = await chrome.cookies.getAll({ url: raw });
    if (!Array.isArray(list) || list.length === 0) return "";
    return list.map((c) => `${c.name}=${c.value}`).join("; ");
  } catch {
    return "";
  }
}

/**
 * Cancels a handed-over browser download, then forgets it.
 *
 * The two steps are separate on purpose. Cancelling is a request, not a fact:
 * it can fail (the download may have finished in the same instant), and erasing
 * the history entry regardless is how a genuinely finished file loses its record
 * while the app downloads a second copy. So: check the error, re-read the item,
 * and only erase once it really is interrupted or cancelled.
 *
 * The id is also recorded in session storage first, because the service worker
 * can be stopped between the handover and this call: on restart the pending list
 * is drained and the leftover browser download is cancelled then.
 */
async function cancelHandedOver(id) {
  await rememberPendingCancel(id);
  const failed = await new Promise((resolve) => {
    chrome.downloads.cancel(id, () => {
      const err = chrome.runtime.lastError;
      resolve(Boolean(err));
    });
  });
  if (failed) {
    // Most often: it completed while we were talking to the app. Leave the
    // history entry alone — the file is real and the app will notice the
    // duplicate — and drop the pending marker.
    await forgetPendingCancel(id);
    return;
  }

  let state = "";
  try {
    const [current] = await chrome.downloads.search({ id });
    state = current ? current.state : "";
  } catch {
    state = "";
  }
  if (state === "complete") {
    await forgetPendingCancel(id);
    return;
  }
  chrome.downloads.erase({ id }, () => void chrome.runtime.lastError);
  await forgetPendingCancel(id);
}

/** Session-storage list of downloads handed over but not yet cancelled. */
const PENDING_KEY = "pendingCancel";

async function pendingCancelIds() {
  try {
    if (typeof chrome === "undefined" || !chrome.storage || !chrome.storage.session) return [];
    const got = await chrome.storage.session.get(PENDING_KEY);
    const arr = got && got[PENDING_KEY];
    return Array.isArray(arr) ? arr.filter((n) => typeof n === "number") : [];
  } catch {
    return [];
  }
}

async function writePendingCancel(ids) {
  try {
    if (typeof chrome !== "undefined" && chrome.storage && chrome.storage.session) {
      await chrome.storage.session.set({ [PENDING_KEY]: ids.slice(-200) });
    }
  } catch {
    // Best effort: the in-memory flow still works.
  }
}

async function rememberPendingCancel(id) {
  const ids = await pendingCancelIds();
  if (!ids.includes(id)) ids.push(id);
  await writePendingCancel(ids);
}

async function forgetPendingCancel(id) {
  const ids = await pendingCancelIds();
  const next = ids.filter((n) => n !== id);
  if (next.length !== ids.length) await writePendingCancel(next);
}

/**
 * Drains the pending-cancel list at worker startup.
 *
 * A worker stopped between "the app accepted this download" and "the browser
 * entry was cancelled" leaves both copies running: the app fetches the file and
 * the browser keeps its own. Nothing else notices, so this runs on every start.
 */
async function drainPendingCancels() {
  const ids = await pendingCancelIds();
  for (const id of ids) {
    try {
      const [current] = await chrome.downloads.search({ id });
      if (current && current.state !== "complete") {
        await new Promise((resolve) => {
          chrome.downloads.cancel(id, () => {
            void chrome.runtime.lastError;
            resolve();
          });
        });
        chrome.downloads.erase({ id }, () => void chrome.runtime.lastError);
      }
    } catch {
      // Already gone; the marker is dropped below either way.
    }
    await forgetPendingCancel(id);
  }
}

if (hasExtensionApis()) {
  void drainPendingCancels();
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

async function getPolicy(fresh) {
  const now = Date.now();
  if (!fresh && policyCache.value && now - policyCache.at < POLICY_TTL_MS) {
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
 * response headers a moment later — so this polls briefly and asks again. The
 * alternative is sending our own HEAD request, which needs a host permission
 * for every site the user might download from; waiting and no extra
 * permission is the better trade.
 *
 * Unknown stays unknown (-1) rather than failing: dropping it would leak a
 * real file to the browser, but the caller treats -1 as "take", so the
 * minimum-size filter cannot see these. Poll long enough that most headers
 * have arrived before giving up.
 */
async function sizeKb(item) {
  if (item.totalBytes > 0) return item.totalBytes / 1024;

  for (let i = 0; i < 5; i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 300));
    try {
      const [found] = await chrome.downloads.search({ id: item.id });
      if (!found) return -1;
      if (found.totalBytes > 0) return found.totalBytes / 1024;
    } catch {
      // The item may already be gone.
      return -1;
    }
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

  // A download another extension started. Taking it over would be stealing a
  // different tool's work: the user installed that one deliberately, and our
  // "take over every download" rule is about the browser's own downloads.
  if (item.byExtensionId) return false;

  return true;
}

if (hasExtensionApis()) {
  chrome.downloads.onCreated.addListener(async (item) => {
    // The worker may have just restarted; reload the self-started ids first
    // so a fallback download is not mistaken for a new user download.
    await loadOwn();
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

    // Chrome usually does not know the filename at onCreated either — it
    // arrives with the response headers, right around when the size does.
    // Re-read the item so the handover carries the real name; otherwise the
    // app falls back to a URL token and the finished file has no real name.
    // This fresh read doubles as the pre-cancel state check below.
    let filename = item.filename ? item.filename.split(/[\\/]/).pop() : "";
    // The browser already knows the total size in most cases (the response
    // headers arrived before the download item appeared). Passing it along
    // lets the app show the real size the moment the row is created — no
    // "0 B until you press start", and no extra request to learn it.
    let size = Number(item.totalBytes) || 0;
    try {
      const [fresh] = await chrome.downloads.search({ id: item.id });
      if (!fresh) return;
      if (fresh.filename) filename = fresh.filename.split(/[\\/]/).pop();
      if (Number(fresh.totalBytes) > 0) size = Number(fresh.totalBytes);
      // Already finished between onCreated and now: leave it to the browser.
      // Handing it over now would create a second copy in the app.
      if (fresh.state === "complete") return;
    } catch {
      // The item may already be gone; fall back to what onCreated saw.
    }

    // Hand over FIRST, cancel only once the app has clearly accepted
    // (ok, duplicate, or pending-awaiting-confirmation): cancelling first and
    // then failing would lose the download entirely. Pending is handed over,
    // not failed, so it never falls back to a browser download.
    const result = await handOver({
      url,
      filename,
      size,
      referer: item.referrer || "",
      cookie: await cookiesFor(url),
      userAgent: navigator.userAgent || "",
    });

    if (result.ok) {
      await cancelHandedOver(item.id);
      report(result, filename);
      return;
    }

    // The app refused (403/400) or could not be reached. The browser's download
    // is untouched and still running, which is exactly what the user wants:
    // it keeps its progress, its Referer and its chosen directory. This used to
    // cancel it and start a bare re-download, which threw all three away and —
    // when that also failed — left the user with nothing at all. So: report, and
    // change nothing.
    report(result, filename);
  });
}

// ---------------------------------------------------------- context menu ---

/**
 * Pulls the first URL out of free-form text.
 *
 * Selection text is free-form, so leading spaces or surrounding prose are
 * trimmed and the first URL is picked rather than handing the raw highlight
 * to the app, where it would fail the scheme check. A trailing full-width
 * punctuation mark (common when the address is copied out of a Chinese
 * sentence) is not part of the address and is stripped.
 */
function pickUrl(raw) {
  const picked = /https?:[^\s"'<>]+/i.exec((raw || "").trim());
  if (!picked) return "";
  return picked[0].replace(/[。，；：！？）】」’”]+$/u, "");
}

if (
  typeof chrome !== "undefined" &&
  chrome.runtime &&
  chrome.runtime.onInstalled
) {
  chrome.runtime.onInstalled.addListener(() => {
    chrome.contextMenus.removeAll(() => {
      chrome.contextMenus.create({
        id: "vkm-add",
        title: "用 VKMDownload 下载",
        contexts: ["link", "video", "audio", "image", "selection"],
      });
    });
  });
}

if (
  typeof chrome !== "undefined" &&
  chrome.contextMenus &&
  chrome.contextMenus.onClicked
) {
  chrome.contextMenus.onClicked.addListener(async (info, tab) => {
    if (info.menuItemId !== "vkm-add") return;

    const url = pickUrl(info.linkUrl || info.srcUrl || info.selectionText || "");
    if (!/^https?:/i.test(url)) {
      report({ ok: false, error: "这个地址不能下载" }, "");
      return;
    }

    const result = await handOver({
      url,
      filename: nameFromUrl(url),
      referer: tab && tab.url ? tab.url : "",
      cookie: await cookiesFor(url),
      userAgent: navigator.userAgent || "",
    });
    report(result, nameFromUrl(url));
  });
}

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
if (
  typeof chrome !== "undefined" &&
  chrome.runtime &&
  chrome.runtime.onMessage
) {
  chrome.runtime.onMessage.addListener((message, _sender, sendResponse) => {
    if (message === "status") {
      (async () => {
        // Fresh read, bypassing the 3s policy cache, so the popup never
        // paints a stale switch right after the app's settings changed it.
        const policy = await getPolicy(true);
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

        const current = await getPolicy(true);
        const enabled = !(current && current.enabled);

        try {
          const response = await fetch(`${base}/settings`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ enabled }),
          });
          if (response.status === 403) {
            const body = await response.json().catch(() => ({}));
            sendResponse({ refused: true, error: body.error || "HTTP 403" });
            return;
          }
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
}

// ------------------------------------------------------------ test hooks ---
//
// Assigned, never exported: a static export would break the service worker
// (classic script), while node tests read these off globalThis after import.

if (typeof globalThis !== "undefined") {
  try {
    globalThis.__vkmExt = {
      shouldTake,
      hostSkipped,
      hostOf,
      sizeKb,
      nameFromUrl,
      pickUrl,
      handOver,
      report,
      getPolicy,
      trackOwn,
      ownDownloads,
      resetForTest() {
        apiBase = null;
        policyCache = { at: 0, value: null };
        ownDownloads.clear();
        ownLoaded = true;
        // The pairing key is module state too. Leaving it behind made one test's
        // key leak into the next, which is also what a stale worker would do
        // after the user deleted the app's pair.key.
        pairKey = null;
      },
    };
  } catch {
    // globalThis sealed or frozen: hooks are test-only, ignore.
  }
}
