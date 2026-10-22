/**
 * Decision-logic tests for the extension service worker.
 *
 * No dependencies: node:test + node:assert only. background.js registers no
 * listeners when `chrome` is undefined, so requiring it here is side-effect
 * free; the functions under test are read off `globalThis.__vkmExt`.
 * Chrome APIs are stub-injected per test via `globalThis.chrome`.
 */

"use strict";

const { describe, it, beforeEach } = require("node:test");
const assert = require("node:assert/strict");

require("../background.js");

const ext = globalThis.__vkmExt;
if (!ext) throw new Error("background.js did not publish __vkmExt hooks");

/** Installs a minimal chrome stub; search/storage behaviour per test. */
function stubChrome({ searchImpl } = {}) {
  globalThis.chrome = {
    storage: {
      local: {
        get: async () => ({}),
        set: async () => {},
        remove: async () => {},
      },
      session: {
        get: async () => ({}),
        set: async () => {},
      },
    },
    downloads: {
      search: searchImpl || (async () => []),
    },
  };
}

const TEST_KEY = "0123456789abcdef".repeat(4);

/** Answer a /ping the way the real app does: HMAC of the challenge.
 *
 *  `extra` adds the protocol handshake fields, so a status test can describe an
 *  app that expects a newer extension than this one. */
async function proofResponse(url, extra = {}) {
  const challenge = new URL(url).searchParams.get("challenge") || "";
  const keyBytes = new Uint8Array(TEST_KEY.match(/../g).map((h) => parseInt(h, 16)));
  const key = await crypto.subtle.importKey(
    "raw",
    keyBytes,
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const sig = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(challenge));
  const hex = [...new Uint8Array(sig)].map((b) => b.toString(16).padStart(2, "0")).join("");
  return {
    ok: true,
    status: 200,
    headers: { get: (name) => (name === "X-VKMDownload-Proof" ? hex : null) },
    json: async () => ({ app: "VKMDownload", ...extra }),
  };
}

/** The ping body that never proves anything: the impostor case. */
function impostorResponse() {
  return {
    ok: true,
    status: 200,
    headers: { get: () => null },
    json: async () => ({ app: "VKMDownload" }),
  };
}

/**
 * Stubs fetch for the whole handshake: /pair hands over the key, /ping proves
 * it, /add answers with addBody.
 *
 * The pairing key is stubbed as already stored, so these tests exercise the
 * handover rather than the pairing exchange itself.
 */
function stubFetch(addBody, opts = {}) {
  const calls = [];
  globalThis.chrome.storage.local.get = async () =>
    opts.noKey ? {} : { vkmPairKey: TEST_KEY };
  globalThis.fetch = async (url, fetchOpts) => {
    calls.push({ url: String(url), opts: fetchOpts });
    const u = String(url);
    if (u.includes("/ping")) return proofResponse(u);
    if (u.endsWith("/pair")) {
      return {
        ok: !opts.pairRefused,
        status: opts.pairRefused ? 403 : 200,
        headers: { get: () => null },
        json: async () => (opts.pairRefused ? { error: "no" } : { key: TEST_KEY }),
      };
    }
    if (u.endsWith("/add")) {
      return { ok: true, status: 200, headers: { get: () => null }, json: async () => addBody };
    }
    throw new Error(`unexpected fetch: ${url}`);
  };
  return calls;
}

beforeEach(() => {
  delete globalThis.chrome;
  ext.resetForTest();
});

describe("shouldTake", () => {
  it("takes plain https downloads", () => {
    assert.equal(ext.shouldTake({ id: 1, url: "https://example.com/f.zip" }), true);
  });

  it("prefers finalUrl over url", () => {
    assert.equal(ext.shouldTake({ id: 1, url: "blob:https://x", finalUrl: "https://cdn.example.com/f.zip" }), true);
    assert.equal(ext.shouldTake({ id: 1, url: "https://example.com/f.zip", finalUrl: "blob:https://x" }), false);
  });

  it("rejects blob:, data: and file: URLs", () => {
    for (const url of ["blob:https://example.com/1", "data:text/plain,hi", "file:///C:/a.zip", ""]) {
      assert.equal(ext.shouldTake({ id: 1, url }), false, url || "(empty)");
    }
  });

  it("skips downloads the extension itself started", () => {
    ext.trackOwn(42);
    assert.equal(ext.shouldTake({ id: 42, url: "https://example.com/f.zip" }), false);
    assert.equal(ext.shouldTake({ id: 43, url: "https://example.com/f.zip" }), true);
  });
});

describe("hostSkipped", () => {
  it("matches exact hosts and subdomains", () => {
    assert.equal(ext.hostSkipped("example.com", ["example.com"]), true);
    assert.equal(ext.hostSkipped("cdn.example.com", ["example.com"]), true);
    assert.equal(ext.hostSkipped("www.example.com", ["example.com"]), true);
  });

  it("rejects non-matches and bad input", () => {
    assert.equal(ext.hostSkipped("example.com", ["other.com"]), false);
    assert.equal(ext.hostSkipped("notexample.com", ["example.com"]), false);
    assert.equal(ext.hostSkipped("", ["example.com"]), false);
    assert.equal(ext.hostSkipped("example.com", null), false);
    assert.equal(ext.hostSkipped("example.com", "example.com"), false);
  });
});

describe("hostOf", () => {
  it("lower-cases and strips www.", () => {
    assert.equal(ext.hostOf("https://WWW.Example.COM/f"), "example.com");
    assert.equal(ext.hostOf("not a url"), "");
  });
});

describe("sizeKb", () => {
  it("uses totalBytes directly when known", async () => {
    stubChrome();
    assert.equal(await ext.sizeKb({ id: 1, totalBytes: 2048 }), 2);
  });

  it("polls search until the size arrives", async () => {
    let n = 0;
    stubChrome({
      searchImpl: async () => (++n < 3 ? [{ totalBytes: 0 }] : [{ totalBytes: 3072 }]),
    });
    assert.equal(await ext.sizeKb({ id: 1, totalBytes: 0 }), 3);
  });

  it("returns -1 when the size never arrives", async () => {
    stubChrome({ searchImpl: async () => [{ totalBytes: 0 }] });
    assert.equal(await ext.sizeKb({ id: 1, totalBytes: 0 }), -1);
  });

  it("returns -1 when the item is gone", async () => {
    stubChrome({ searchImpl: async () => [] });
    assert.equal(await ext.sizeKb({ id: 1, totalBytes: 0 }), -1);
  });
});

describe("pickUrl", () => {
  it("picks the first URL out of prose with leading spaces", () => {
    assert.equal(
      ext.pickUrl("  请下载 https://example.com/f.zip 谢谢"),
      "https://example.com/f.zip",
    );
  });

  it("strips trailing full-width punctuation", () => {
    for (const mark of ["。", "，", "；", "：", "！", "？", "）", "】", "」", "’", "”"]) {
      assert.equal(ext.pickUrl(`地址https://example.com/f.zip${mark}`), "https://example.com/f.zip", mark);
    }
    assert.equal(ext.pickUrl("看https://example.com/f.zip！！"), "https://example.com/f.zip");
  });

  it("keeps interior punctuation and returns empty when none", () => {
    assert.equal(ext.pickUrl("https://example.com/a?b=1;c=2"), "https://example.com/a?b=1;c=2");
    assert.equal(ext.pickUrl("没有地址"), "");
  });
});

describe("handOver", () => {
  it("maps {pending:true} to handed-over-awaiting-confirmation", async () => {
    stubChrome();
    stubFetch({ pending: true, name: "f.zip" });
    const result = await ext.handOver({ url: "https://example.com/f.zip" });
    assert.deepEqual(result, { ok: true, pending: true, name: "f.zip" });
  });

  it("still maps duplicates", async () => {
    stubChrome();
    stubFetch({ duplicate: true, id: "t1", name: "f.zip" });
    const result = await ext.handOver({ url: "https://example.com/f.zip" });
    assert.deepEqual(result, { ok: true, duplicate: true, name: "f.zip" });
  });

  it("reports a peer-check 403 with the app's reason", async () => {
    stubChrome();
    stubFetch({});
    globalThis.fetch = async (url) => {
      const u = String(url);
      if (u.includes("/ping")) return proofResponse(u);
      if (u.endsWith("/pair")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ key: TEST_KEY }) };
      }
      return {
        ok: false,
        status: 403,
        headers: { get: () => null },
        json: async () => ({ error: "caller is not a recognized browser (evil.exe)" }),
      };
    };
    const result = await ext.handOver({ url: "https://example.com/f.zip" });
    assert.equal(result.ok, false);
    assert.equal(result.refused, true);
    assert.match(result.error, /not a recognized browser/);
  });
});

describe("intercept fallback order", () => {
  // Regression: the failure path used to re-download while the original
  // browser entry was still alive, fetching the file twice.
  it("cancels the original before re-downloading on refusal", async () => {
    const order = [];
    let createdHandler;
    const savedFetch = globalThis.fetch;
    globalThis.chrome = {
      storage: {
        local: { get: async () => ({}), set: async () => {}, remove: async () => {} },
        session: { get: async () => ({}), set: async () => {} },
      },
      downloads: {
        search: async () => [
          { id: 7, state: "in_progress", filename: "C:\\dl\\f.zip", totalBytes: 5 * 1024 * 1024 },
        ],
        cancel: (id, cb) => {
          order.push("cancel");
          cb();
        },
        erase: (id, cb) => cb(),
        download: async () => {
          order.push("download");
          return 8;
        },
        onCreated: {
          addListener: (fn) => {
            createdHandler = fn;
          },
        },
        onChanged: { addListener: () => {} },
      },
      cookies: { getAll: async () => [] },
      notifications: { create: () => {} },
      contextMenus: { removeAll: (cb) => cb(), create: () => {} },
      runtime: {
        onMessage: { addListener: () => {} },
        onInstalled: { addListener: () => {} },
      },
    };
    globalThis.chrome.storage.local.get = async () => ({ vkmPairKey: TEST_KEY });
    globalThis.fetch = async (url) => {
      const u = String(url);
      if (u.includes("/ping")) {
        return proofResponse(u);
      }
      if (u.endsWith("/pair")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ key: TEST_KEY }) };
      }
      if (u.endsWith("/settings")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ enabled: true, minSizeKb: 0, skipHosts: [] }) };
      }
      if (u.endsWith("/add")) {
        return { ok: false, status: 400, headers: { get: () => null }, json: async () => ({ error: "nope" }) };
      }
      throw new Error(`unexpected fetch: ${u}`);
    };
    try {
      delete require.cache[require.resolve("../background.js")];
      require("../background.js");
      const ext2 = globalThis.__vkmExt;
      ext2.resetForTest();
      await createdHandler({ id: 7, url: "https://example.com/f.zip", totalBytes: 5 * 1024 * 1024 });
      // A refusal leaves the browser's own download running. Cancelling it and
      // starting a fresh one (what this used to assert) threw away the
      // progress, the Referer and the chosen directory for no benefit.
      assert.deepEqual(order, [], "a refusal must not cancel or re-download");
    } finally {
      globalThis.fetch = savedFetch;
      delete require.cache[require.resolve("../background.js")];
      require("../background.js");
    }
  });
});

// -------------------------------------------------- intercept: the filename
// Regression: a download handed over while Chrome had not yet filled in the
// filename reached the app with an empty name, so from the second browser
// download onwards the placement dialog opened with a blank filename field.
// The handover must retry briefly, then fall back to the URL's last segment.

/** Full chrome stub plus a fetch that answers /settings and records /add.
 *
 *  `policy` is the /settings body: an app from before the sendCookies /
 *  incognito switches simply omits them, which is what the default here does.
 *  `pingBody` adds the /ping protocol handshake for the status tests. */
function stubIntercept({ searchImpl, onAdd, policy, pingBody } = {}) {
  const state = stubFullChrome();
  // A download that already has a filename, so the retry loop stops on its
  // first read and these tests do not wait the full 300ms.
  globalThis.chrome.downloads.search =
    searchImpl ||
    (async () => [{ state: "in_progress", filename: "C:\\dl\\file.bin", totalBytes: 4096 }]);
  globalThis.chrome.storage.local.get = async () => ({ vkmPairKey: TEST_KEY });
  let cookieReads = 0;
  globalThis.chrome.cookies.getAll = async () => {
    cookieReads += 1;
    return [{ name: "session", value: "s3cret" }];
  };
  const calls = [];
  globalThis.fetch = async (url, opts) => {
    const u = String(url);
    calls.push({ url: u, body: opts && opts.body });
    if (u.includes("/ping")) return proofResponse(u, pingBody);
    if (u.endsWith("/pair")) {
      return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ key: TEST_KEY }) };
    }
    if (u.endsWith("/settings")) {
      return {
        ok: true,
        status: 200,
        headers: { get: () => null },
        json: async () => policy || { enabled: true, minSizeKb: 0, skipHosts: [] },
      };
    }
    if (u.endsWith("/add")) {
      if (onAdd) onAdd();
      return {
        ok: true,
        status: 200,
        headers: { get: () => null },
        json: async () => ({ pending: true, name: "fallback.bin" }),
      };
    }
    throw new Error(`unexpected fetch: ${u}`);
  };
  return { state, calls, cookieReads: () => cookieReads };
}

describe("intercept filename", () => {
  it("retries search, then falls back to the URL name", async () => {
    let searches = 0;
    let searchesAtAdd = 0;
    // What Chrome answers while the response headers have not arrived: the
    // item exists, it just has no filename yet.
    const { state, calls } = stubIntercept({
      searchImpl: async () => {
        searches += 1;
        return [{ state: "in_progress", totalBytes: 4096 }];
      },
      // Read at handover time: cancelling the browser entry searches again, so
      // the final count would not say how many reads the name lookup made.
      onAdd: () => {
        searchesAtAdd = searches;
      },
    });
    rerequire();
    assert.equal(typeof state.created, "function", "onCreated listener must be registered");

    await state.created({ id: 9, url: "https://example.com/dl/fallback.bin", totalBytes: 4096 });

    const add = calls.find((c) => c.url.endsWith("/add"));
    assert.ok(add, "the download must be handed over");
    assert.equal(
      JSON.parse(add.body).filename,
      "fallback.bin",
      "an unnamed download must fall back to the last URL path segment",
    );
    assert.equal(searchesAtAdd, 3, `search was polled ${searchesAtAdd} times, want the full retry`);
  });

  it("uses the name as soon as search returns it", async () => {
    let searches = 0;
    let searchesAtAdd = 0;
    const { state, calls } = stubIntercept({
      searchImpl: async () => {
        searches += 1;
        // The headers land between the first and the second read.
        return searches === 1
          ? [{ state: "in_progress", totalBytes: 4096 }]
          : [{ state: "in_progress", filename: "C:\\dl\\real-name.bin", totalBytes: 4096 }];
      },
      onAdd: () => {
        searchesAtAdd = searches;
      },
    });
    rerequire();

    await state.created({ id: 9, url: "https://example.com/dl/url-name.bin", totalBytes: 4096 });

    const add = calls.find((c) => c.url.endsWith("/add"));
    assert.ok(add, "the download must be handed over");
    assert.equal(JSON.parse(add.body).filename, "real-name.bin");
    assert.equal(searchesAtAdd, 2, "the poll must stop once the name arrives");
  });

  it("nameFromUrl takes the last segment, decoded", () => {
    assert.equal(ext.nameFromUrl("https://example.com/a/%E6%96%87%E4%BB%B6.zip?x=1"), "文件.zip");
    assert.equal(ext.nameFromUrl("https://example.com/"), "");
    assert.equal(ext.nameFromUrl("not a url"), "");
  });
});

// ------------------------------------------------- coverage: report/policy/own
// These pin the paths the earlier suites never asserted: the three report
// branches, the policy cache TTL, and the own-download bookkeeping.

/** Full chrome stub: enough surface for background.js to register every
 *  listener, with the handlers captured for direct invocation. */
function stubFullChrome() {
  const state = { msg: null, changed: null, created: null, notes: [] };
  globalThis.chrome = {
    storage: {
      local: { get: async () => ({}), set: async () => {}, remove: async () => {} },
      session: { get: async () => ({}), set: async () => {} },
    },
    downloads: {
      search: async () => [],
      cancel: (_id, cb) => cb && cb(),
      erase: (_id, cb) => cb && cb(),
      download: async () => 1,
      onCreated: { addListener: (fn) => { state.created = fn; } },
      onChanged: { addListener: (fn) => { state.changed = fn; } },
    },
    cookies: { getAll: async () => [] },
    notifications: { create: (o) => state.notes.push(o) },
    contextMenus: { removeAll: (cb) => cb && cb(), create: () => {} },
    runtime: {
      onMessage: { addListener: (fn) => { state.msg = fn; } },
      onInstalled: { addListener: () => {} },
      // The status message reports the extension's own version; the popup
      // compares it with the app's minimum.
      getManifest: () => ({ version: "1.0.3" }),
    },
  };
  return state;
}

/** Re-requires background.js so it registers listeners against the stub. */
function rerequire() {
  delete require.cache[require.resolve("../background.js")];
  require("../background.js");
  return globalThis.__vkmExt;
}

describe("report", () => {
  it("distinguishes pending, duplicate, handed over and refused", () => {
    const notes = [];
    globalThis.chrome = { notifications: { create: (o) => notes.push(o) } };

    ext.report({ pending: true, name: "a.zip" }, "fallback");
    ext.report({ duplicate: true }, "b.zip");
    ext.report({ ok: true }, "c.zip");
    ext.report({ ok: false, refused: true, error: "已拒绝" }, "fallback");
    ext.report({ ok: false, error: "打不通" }, "fallback");

    assert.equal(notes[0].title, "已转到应用等确认");
    assert.equal(notes[0].message, "a.zip");
    assert.equal(notes[1].title, "已在下载列表中");
    assert.equal(notes[1].message, "b.zip");
    assert.equal(notes[2].title, "已交给 VKMDownload");
    assert.equal(notes[3].title, "VKMDownload 没有响应");
    assert.equal(notes[3].message, "已拒绝");
    assert.equal(notes[3].priority, 2, "a refusal is urgent, not chatty");
    assert.equal(notes[4].title, "VKMDownload 没有响应");
    assert.ok(notes[4].message.includes("请先启动 VKMDownload"), notes[4].message);
  });
});

// ------------------------------------------------- extension policy switches
// The app owns the policy. These cover the two behavioural switches it sends,
// including what an app from before them (no keys at all) must mean.

describe("policy switches", () => {
  it("fills the historical defaults for an app that does not send them", async () => {
    stubChrome();
    globalThis.chrome.storage.local.get = async () => ({ vkmPairKey: TEST_KEY });
    globalThis.fetch = async (url) => {
      const u = String(url);
      if (u.includes("/ping")) return proofResponse(u);
      if (u.endsWith("/pair")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ key: TEST_KEY }) };
      }
      if (u.endsWith("/settings")) {
        return {
          ok: true,
          status: 200,
          headers: { get: () => null },
          json: async () => ({ enabled: true, minSizeKb: 1024, skipHosts: ["example.com"] }),
        };
      }
      throw new Error(`unexpected fetch: ${u}`);
    };

    const policy = await ext.getPolicy(true);
    assert.equal(policy.sendCookies, true, "an absent key must mean cookies on");
    assert.equal(policy.incognito, false, "an absent key must mean incognito off");
    assert.equal(policy.enabled, true);
    assert.equal(policy.minSizeKb, 1024);
    assert.deepEqual(policy.skipHosts, ["example.com"]);
  });

  it("keeps an explicit false/true and still defaults the others", () => {
    const explicit = ext.normalizePolicy({ enabled: true, sendCookies: false, incognito: true });
    assert.equal(explicit.sendCookies, false);
    assert.equal(explicit.incognito, true);

    const missing = ext.normalizePolicy({ enabled: true });
    assert.equal(missing.sendCookies, true);
    assert.equal(missing.incognito, false);
    assert.equal(missing.minSizeKb, 0);
    assert.deepEqual(missing.skipHosts, []);
  });
});

describe("cookie handover policy", () => {
  it("does not read or send cookies when the app turned them off", async () => {
    const { state, calls, cookieReads } = stubIntercept({
      policy: { enabled: true, minSizeKb: 0, skipHosts: [], sendCookies: false },
    });
    rerequire();

    await state.created({ id: 11, url: "https://example.com/files/private.bin", totalBytes: 4096 });

    const add = calls.find((c) => c.url.endsWith("/add"));
    assert.ok(add, "the download must still be handed over");
    assert.equal(JSON.parse(add.body).cookie, "", "no login session may be sent");
    assert.equal(cookieReads(), 0, "the cookies must not even be read");
  });

  it("reads and sends them by default, including for an older app", async () => {
    const { state, calls, cookieReads } = stubIntercept({
      policy: { enabled: true, minSizeKb: 0, skipHosts: [] },
    });
    rerequire();

    await state.created({ id: 12, url: "https://example.com/files/private.bin", totalBytes: 4096 });

    const add = calls.find((c) => c.url.endsWith("/add"));
    assert.ok(add);
    assert.equal(JSON.parse(add.body).cookie, "session=s3cret");
    assert.equal(cookieReads(), 1);
  });
});

describe("incognito handover policy", () => {
  it("leaves a private download to the browser by default", async () => {
    const { state, calls } = stubIntercept(); // the default policy has no incognito key
    rerequire();

    await state.created({ id: 13, incognito: true, url: "https://example.com/a.bin", totalBytes: 4096 });

    assert.equal(
      calls.filter((c) => c.url.endsWith("/add")).length,
      0,
      "a private download must not be handed over",
    );
  });

  it("takes it over when the app allows incognito", async () => {
    const { state, calls } = stubIntercept({
      policy: { enabled: true, minSizeKb: 0, skipHosts: [], incognito: true },
    });
    rerequire();

    await state.created({ id: 14, incognito: true, url: "https://example.com/a.bin", totalBytes: 4096 });

    assert.equal(calls.filter((c) => c.url.endsWith("/add")).length, 1);
  });

  it("still takes normal windows, whatever the incognito switch says", async () => {
    const { state, calls } = stubIntercept();
    rerequire();

    await state.created({ id: 15, url: "https://example.com/a.bin", totalBytes: 4096 });

    assert.equal(calls.filter((c) => c.url.endsWith("/add")).length, 1);
  });
});

describe("popup status", () => {
  it("carries the app's minimum version and warns when this build is older", async () => {
    const { state } = stubIntercept({ pingBody: { protocol: 1, minExtensionVersion: "9.9.9" } });
    rerequire();

    const resp = await new Promise((resolve) => state.msg("status", {}, resolve));
    assert.equal(resp.running, true);
    assert.equal(resp.enabled, true);
    assert.equal(resp.extVersion, "1.0.3");
    assert.equal(resp.minExtensionVersion, "9.9.9");
    assert.equal(resp.versionMismatch, true);
  });

  it("does not warn when the extension meets the minimum", async () => {
    const { state } = stubIntercept({ pingBody: { protocol: 1, minExtensionVersion: "1.0.3" } });
    rerequire();

    const resp = await new Promise((resolve) => state.msg("status", {}, resolve));
    assert.equal(resp.minExtensionVersion, "1.0.3");
    assert.equal(resp.versionMismatch, false);
  });

  it("has no toggle any more: the settings belong to the app", () => {
    const { state } = stubIntercept();
    rerequire();

    let answered = false;
    const handled = state.msg("toggle", {}, () => {
      answered = true;
    });
    assert.equal(handled, false, "the toggle message must no longer be handled");
    assert.equal(answered, false, "and must not answer");
  });
});

describe("popup is read-only", () => {
  const fs = require("node:fs");
  const path = require("node:path");
  const root = path.join(__dirname, "..");

  it("renders a status line, not a switch, and points at the app", () => {
    const html = fs.readFileSync(path.join(root, "popup.html"), "utf8");
    assert.ok(!/id="toggle"/.test(html), "the popup must not render a toggle control");
    assert.ok(html.includes("设置 → 浏览器扩展"), "the popup must say where the switch lives");
    assert.ok(/id="verwarn"/.test(html), "the version warning line must exist");
  });

  it("never writes the settings over the endpoint", () => {
    const popup = fs.readFileSync(path.join(root, "popup.js"), "utf8");
    assert.ok(!popup.includes('"toggle"'), "popup.js must not ask for a toggle");

    const background = fs.readFileSync(path.join(root, "background.js"), "utf8");
    assert.ok(!background.includes('message === "toggle"'), "background.js must not handle a toggle");
    assert.ok(!background.includes('"POST", "/settings"'), "background.js must not POST the settings");
  });
});

describe("getPolicy cache", () => {
  it("serves the 3s cache, refetches after it expires, and bypasses on fresh", async () => {
    stubChrome();
    globalThis.chrome.storage.local.get = async () => ({ vkmPairKey: TEST_KEY });
    let settings = 0;
    globalThis.fetch = async (url) => {
      const u = String(url);
      if (u.includes("/ping")) {
        return proofResponse(u);
      }
      if (u.endsWith("/pair")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ key: TEST_KEY }) };
      }
      if (u.endsWith("/settings")) {
        settings++;
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ enabled: true, minSizeKb: 0, skipHosts: [] }) };
      }
      throw new Error(`unexpected fetch: ${u}`);
    };

    let fake = 10_000_000;
    const realNow = Date.now;
    Date.now = () => fake;
    try {
      await ext.getPolicy();
      await ext.getPolicy();
      assert.equal(settings, 1, "a read inside the TTL must come from the cache");
      fake += 4000;
      await ext.getPolicy();
      assert.equal(settings, 2, "an expired cache must refetch");
      await ext.getPolicy(true);
      assert.equal(settings, 3, "fresh=true must bypass the cache");
    } finally {
      Date.now = realNow;
    }
  });
});

describe("own download tracking", () => {
  it("caps the remembered ids at 1000 and forgets settled ones", () => {
    const state = stubFullChrome();
    const fresh = rerequire();

    for (let i = 1; i <= 1001; i++) fresh.trackOwn(i);
    assert.equal(fresh.ownDownloads.size, 1000, "the cap must hold");
    assert.ok(fresh.ownDownloads.has(1001), "the newest id must survive");
    assert.ok(!fresh.ownDownloads.has(1), "the oldest id must be evicted first");

    assert.equal(typeof state.changed, "function", "onChanged listener must be registered");
    state.changed({ id: 1001, state: { current: "complete" } });
    assert.ok(!fresh.ownDownloads.has(1001), "a completed own download must be forgotten");
  });
});
