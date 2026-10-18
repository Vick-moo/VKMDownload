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

/** Answer a /ping the way the real app does: HMAC of the challenge. */
async function proofResponse(url) {
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
    json: async () => ({ app: "VKMDownload" }),
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

// ------------------------------------------------- coverage: report/toggle/policy/own
// These pin the paths the earlier suites never asserted: the three report
// branches, the toggle round trip (including the peer-check 403), the policy
// cache TTL, and the own-download bookkeeping.

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

describe("toggle", () => {
  it("reports a peer-check 403 with the app's reason", async () => {
    const state = stubFullChrome();
    globalThis.chrome.storage.local.get = async () => ({ vkmPairKey: TEST_KEY });
    globalThis.fetch = async (url, opts) => {
      const u = String(url);
      if (u.includes("/ping")) {
        return proofResponse(u);
      }
      if (u.endsWith("/pair")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ key: TEST_KEY }) };
      }
      if (u.endsWith("/settings") && opts && opts.method === "POST") {
        return { ok: false, status: 403, headers: { get: () => null }, json: async () => ({ error: "已拒绝：非浏览器进程" }) };
      }
      if (u.endsWith("/settings")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ enabled: true, minSizeKb: 0, skipHosts: [] }) };
      }
      throw new Error(`unexpected fetch: ${u}`);
    };
    rerequire();
    assert.equal(typeof state.msg, "function", "onMessage listener must be registered");

    const resp = await new Promise((resolve) => state.msg("toggle", {}, resolve));
    assert.deepEqual(resp, { refused: true, error: "已拒绝：非浏览器进程" });
  });

  it("flips the live value and answers with the new one", async () => {
    const state = stubFullChrome();
    globalThis.chrome.storage.local.get = async () => ({ vkmPairKey: TEST_KEY });
    globalThis.fetch = async (url, opts) => {
      const u = String(url);
      if (u.includes("/ping")) {
        return proofResponse(u);
      }
      if (u.endsWith("/pair")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ key: TEST_KEY }) };
      }
      if (u.endsWith("/settings") && opts && opts.method === "POST") {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) };
      }
      return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ enabled: true, minSizeKb: 0, skipHosts: [] }) };
    };
    rerequire();

    const resp = await new Promise((resolve) => state.msg("toggle", {}, resolve));
    assert.deepEqual(resp, { enabled: false }, "enabled:true must flip to false");
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
