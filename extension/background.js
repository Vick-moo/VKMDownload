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
  // null forgets the key (used by 重新配对); a real key replaces it.
  pairKey = key || null;
  if (!key) return;
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

const AUTH_HEADER = "X-VKMDownload-Auth";

/**
 * Signs a mutating request: HMAC(key, method + "\n" + path + "\n" + unix).
 *
 * The peer check proves the caller is a real browser, and the pairing proof
 * proves the responder is the real app. Neither says anything about which
 * extension is calling: any installed extension is also "a browser". Since /add
 * enqueues a download with caller-supplied cookies and URL, the app demands this
 * signature, and only a holder of the pairing key can produce it.
 *
 * A timestamp inside the signature gives the app a two-minute window, which is
 * what keeps the endpoint stateless. Returns "" when there is no key, in which
 * case the caller must not send the request at all.
 */
async function signRequest(method, path) {
  const key = await loadPairKey();
  if (!key) return "";
  const stamp = String(Math.floor(Date.now() / 1000));
  const mac = await hmacHex(key, `${method}\n${path}\n${stamp}`);
  return `${stamp}:${mac}`;
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
  if (!key) {
    // First pairing only. Asking now is safe *because* nothing is trusted yet,
    // and the app answers /pair only to a signed browser process.
    //
    // Once a key is stored it is never silently replaced. The tempting "if the
    // old key fails, just ask again" would hand the whole scheme to an attacker:
    // any program that binds the port first could answer /pair with its own key,
    // and the extension would happily re-pin to it. A lost key (the app was
    // reinstalled, or pair.key was deleted) therefore looks exactly like "the app
    // is not running" until the user presses 重新配对 in the popup.
    for (const base of candidates) {
      const got = await pairWith(base);
      if (!got) continue;
      if (await provePort(base, got)) {
        await savePairKey(got);
        apiBase = base;
        return apiBase;
      }
    }
    return null;
  }

  for (const base of candidates) {
    if (await provePort(base, key)) {
      apiBase = base;
      return apiBase;
    }
  }

  // Nothing proved itself. Do not cache anything: the real app may start later,
  // and the stored key is still valid for it.
  return null;
}

/**
 * Returns a base URL that has just proven it holds the key.
 *
 * The cached one is not trusted as-is. The attack is cheap and concrete: the app
 * quits, any program on the machine binds the port it used, and a cached base
 * would receive the next download's URL and cookies without ever seeing a
 * challenge. Re-proving costs one loopback round trip per handover — a few
 * milliseconds — which is the right price for not handing a credential to
 * whatever got to the port first.
 */
async function ensureApp() {
  const cached = apiBase;
  if (cached) {
    const key = await loadPairKey();
    if (key && (await provePort(cached, key))) return cached;
    // It stopped proving itself: forget the port, then rediscover.
    apiBase = null;
  }
  return findApp();
}

/** Hands one download to the app. */
async function handOver(payload) {
  const base = await ensureApp();
  if (!base) {
    return { ok: false, error: "VKMDownload 没有运行" };
  }

  // Signed before the body is built, so a caller without the key never sends
  // the URL or the cookie at all.
  const auth = await signRequest("POST", "/add");
  if (!auth) {
    return { ok: false, error: "还没有和 VKMDownload 完成配对" };
  }

  try {
    const response = await fetch(`${base}/add`, {
      method: "POST",
      headers: { "Content-Type": "application/json", [AUTH_HEADER]: auth },
      body: JSON.stringify(payload),
      // A redirect would carry a body containing cookies and a signed URL
      // somewhere else entirely. The app never redirects.
      redirect: "error",
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
  // Everything shown here is built from server- and page-supplied strings, and a
  // notification is the most public place the app has: it appears on the lock
  // screen and stays in the notification centre. sanitizeText strips URLs down
  // to host and path and drops control characters.
  const name = sanitizeText(result.name || fallbackName || "", 80);
  const err = sanitizeText(result.error || "", 120);
  if (result.pending) {
    chrome.notifications.create({
      type: "basic",
      iconUrl: "icons/icon128.png",
      title: "已转到应用等确认",
      message: name || "已转到应用，等你在弹窗里确认位置",
      priority: 0,
    });
    return;
  }
  if (result.duplicate) {
    chrome.notifications.create({
      type: "basic",
      iconUrl: "icons/icon128.png",
      title: "已在下载列表中",
      message: name || "该链接已在列表中，无需重复添加",
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
      ? name || "已加入下载队列"
      : result.refused
        ? err
        : `${err} · 请先启动 VKMDownload`,
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
  // Never for a loopback or private address. The manifest grants
  // http://127.0.0.1/* so the extension can reach the app, which incidentally
  // makes a local development server's session cookies readable — and no
  // download on a private address needs the user's login.
  if (isPrivateHost(hostOf(raw))) return "";
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

/** True for loopback, link-local and RFC1918 hosts. */
function isPrivateHost(host) {
  if (!host) return false;
  if (host === "localhost" || host.endsWith(".localhost")) return true;
  const v4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(host);
  if (v4) {
    const a = Number(v4[1]);
    const b = Number(v4[2]);
    if (a === 127 || a === 10) return true;
    if (a === 192 && b === 168) return true;
    if (a === 172 && b >= 16 && b <= 31) return true;
    if (a === 169 && b === 254) return true;
    return false;
  }
  if (host === "::1" || host.startsWith("fc") || host.startsWith("fd") || host.startsWith("fe80")) return true;
  return false;
}

/**
 * The part of a page URL that is safe to replay as a Referer.
 *
 * A full page URL carries the user's search terms and document ids in its query
 * string, and it ends up in the app's task record. Origin plus path is what a
 * host needs to see the request as same-site; the query is not.
 */
function refererFor(raw) {
  if (!raw) return "";
  try {
    const u = new URL(raw);
    if (u.protocol !== "http:" && u.protocol !== "https:") return "";
    return `${u.origin}${u.pathname}`;
  } catch {
    return "";
  }
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

/**
 * How long the handover waits for Chrome to fill in a download's name.
 *
 * Chrome knows the filename only after the response headers arrive, which is a
 * moment after onCreated. A single immediate read used to be the whole attempt,
 * and the second and later handovers of a session — faster, because pairing and
 * the policy are cached by then — lost that race and sent an empty name to the
 * app, which opened the placement dialog with an empty filename. Three short
 * retries cover the usual delay without holding the download up for long.
 */
const FILENAME_TRIES = 3;
const FILENAME_RETRY_MS = 150;

if (hasExtensionApis()) {
  chrome.downloads.onCreated.addListener(async (item) => {
    // The worker may have just restarted; reload the self-started ids first
    // so a fallback download is not mistaken for a new user download.
    await loadOwn();
    // An incognito download must not be handed over: the app keeps a task record,
    // a log line and a notification, and none of them belong to a session the
    // user asked to leave no trace of.
    if (item.incognito) return;
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
    // Read the item, and keep reading briefly while it has no name: the name
    // can still be missing on the first read (see FILENAME_TRIES). The first
    // read always happens, so the size and the finished-state check below are
    // still made even when onCreated already knew the name.
    for (let attempt = 0; attempt < FILENAME_TRIES; attempt += 1) {
      if (attempt > 0) {
        await new Promise((resolve) => setTimeout(resolve, FILENAME_RETRY_MS));
      }
      try {
        const [fresh] = await chrome.downloads.search({ id: item.id });
        if (!fresh) return;
        if (fresh.filename) filename = fresh.filename.split(/[\\/]/).pop();
        if (Number(fresh.totalBytes) > 0) size = Number(fresh.totalBytes);
        // Already finished between onCreated and now: leave it to the browser.
        // Handing it over now would create a second copy in the app.
        if (fresh.state === "complete") return;
        if (filename) break;
      } catch {
        // The item may already be gone; fall back to what onCreated saw.
        break;
      }
    }
    // Still nameless, so Chrome never filled it in (some servers send no
    // Content-Disposition and the URL has no useful tail). Hand over the URL's
    // last segment, which is the name the app would otherwise have to invent
    // itself, so the placement dialog is prefilled either way.
    if (!filename) filename = nameFromUrl(url);

    // Prove the app before reading the user's cookies, not after. The old order
    // read them into a payload first and only then looked for an app, so a
    // download against a port that never proved itself still caused the
    // cookies to be read — and, with a stale cached port, sent.
    const base = await ensureApp();
    if (!base) {
      // Nothing to hand over to. The browser's own download continues, which is
      // the only safe outcome: cancelling it here would lose the file.
      return;
    }

    // Hand over FIRST, cancel only once the app has clearly accepted
    // (ok, duplicate, or pending-awaiting-confirmation): cancelling first and
    // then failing would lose the download entirely. Pending is handed over,
    // not failed, so it never falls back to a browser download.
    const result = await handOver({
      url,
      filename,
      size,
      referer: refererFor(item.referrer),
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
    if (tab && tab.incognito) return;

    const url = pickUrl(info.linkUrl || info.srcUrl || info.selectionText || "");
    if (!/^https?:/i.test(url)) {
      report({ ok: false, error: "这个地址不能下载" }, "");
      return;
    }

    // Same order as the interception path: prove first, read cookies second.
    const base = await ensureApp();
    if (!base) {
      report({ ok: false, error: "VKMDownload 没有运行" }, nameFromUrl(url));
      return;
    }

    const result = await handOver({
      url,
      filename: nameFromUrl(url),
      referer: refererFor(tab && tab.url ? tab.url : ""),
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

    if (message === "forgetPairing") {
      // The only way back from a lost key: the user asks, having just seen the
      // popup say the app is missing. Silent re-pairing is what the pairing
      // design exists to prevent.
      (async () => {
        await savePairKey(null);
        try {
          if (globalThis.chrome && chrome.storage && chrome.storage.local) {
            await chrome.storage.local.remove(KEY_STORAGE);
          }
        } catch {
          // In-memory clear still applies for this worker.
        }
        apiBase = null;
        policyCache = { at: 0, value: null };
        sendResponse({ cleared: true });
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

        const auth = await signRequest("POST", "/settings");
        if (!auth) {
          sendResponse({ refused: true, error: "扩展还没有和 VKMDownload 配对" });
          return;
        }

        try {
          const response = await fetch(`${base}/settings`, {
            method: "POST",
            headers: { "Content-Type": "application/json", [AUTH_HEADER]: auth },
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
