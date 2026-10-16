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
function stubChrome({ token, searchImpl } = {}) {
  const localData = token === undefined ? {} : { apiToken: token };
  globalThis.chrome = {
    storage: {
      local: {
        get: async () => ({ ...localData }),
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

/** Stubs fetch for findApp (/ping) and handOver (/add); records calls. */
function stubFetch(addBody) {
  const calls = [];
  globalThis.fetch = async (url, opts) => {
    calls.push({ url: String(url), opts });
    if (String(url).endsWith("/ping")) {
      return { ok: true, status: 200, json: async () => ({ app: "VKMDownload" }) };
    }
    if (String(url).endsWith("/add")) {
      return { ok: true, status: 200, json: async () => addBody };
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

  it("attaches Bearer token when paired, omits it when not", async () => {
    stubChrome({ token: "secret-token" });
    let calls = stubFetch({ name: "f.zip" });
    await ext.handOver({ url: "https://example.com/a.zip" });
    assert.equal(calls.find((c) => c.url.endsWith("/add")).opts.headers.Authorization, "Bearer secret-token");

    ext.resetForTest();
    stubChrome();
    calls = stubFetch({ name: "f.zip" });
    await ext.handOver({ url: "https://example.com/b.zip" });
    assert.equal("Authorization" in calls.find((c) => c.url.endsWith("/add")).opts.headers, false);
  });

  it("reports 403 as a pairing failure", async () => {
    stubChrome();
    globalThis.fetch = async (url) => {
      if (String(url).endsWith("/ping")) {
        return { ok: true, status: 200, json: async () => ({ app: "VKMDownload" }) };
      }
      return { ok: false, status: 403, json: async () => ({}) };
    };
    const result = await ext.handOver({ url: "https://example.com/f.zip" });
    assert.equal(result.ok, false);
    assert.equal(result.pairError, true);
    assert.match(result.error, /配对失败/);
  });
});
