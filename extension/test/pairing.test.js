/**
 * Pairing tests: the extension must not hand a URL or a cookie to a program that
 * merely answers on the right port.
 *
 * The threat is concrete: any local process can bind 18888 and reply with
 * {"app":"VKMDownload"}. Before pairing, the extension accepted that and posted
 * the user's cookies (readable through the cookies API, HttpOnly included) and a
 * signed download URL straight to it. Now the port has to prove it holds the
 * per-install key, and anything that cannot is skipped.
 */

"use strict";

const { describe, it, beforeEach } = require("node:test");
const assert = require("node:assert/strict");

require("../background.js");

const ext = globalThis.__vkmExt;
if (!ext) throw new Error("background.js did not publish __vkmExt hooks");

const TEST_KEY = "a".repeat(64);

/** Installs a chrome stub with the surface the handover path touches. */
function stubChrome({ storedKey, cookies } = {}) {
  const state = { cancelled: [], notes: [], downloads: [] };
  globalThis.chrome = {
    storage: {
      local: {
        get: async () => (storedKey ? { vkmPairKey: storedKey } : {}),
        set: async () => {},
        remove: async () => {},
      },
      session: { get: async () => ({}), set: async () => {} },
    },
    downloads: {
      search: async () => [],
      cancel: (id, cb) => {
        state.cancelled.push(id);
        if (cb) cb();
      },
      erase: () => {},
      download: async () => 1,
    },
    cookies: { getAll: async () => cookies || [] },
    notifications: { create: (o) => state.notes.push(o) },
    runtime: { lastError: null, onMessage: { addListener: () => {} } },
  };
  return state;
}

/** HMAC helper mirroring the extension's own. */
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

/** A ping answer from a holder of `key`. */
async function pingWith(url, key) {
  const challenge = new URL(url).searchParams.get("challenge") || "";
  const proof = await hmacHex(key, challenge);
  return {
    ok: true,
    status: 200,
    headers: { get: (n) => (n === "X-VKMDownload-Proof" ? proof : null) },
    json: async () => ({ app: "VKMDownload" }),
  };
}

beforeEach(() => {
  delete globalThis.chrome;
  ext.resetForTest();
});

describe("pairing", () => {
  it("refuses a port that answers the ping body but cannot prove the key", async () => {
    const state = stubChrome({ storedKey: TEST_KEY, cookies: [{ name: "sid", value: "SECRET" }] });
    const seen = [];
    globalThis.fetch = async (url) => {
      const u = String(url);
      seen.push(u);
      if (u.includes("/ping")) {
        // The impostor: right body, no proof header.
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ app: "VKMDownload" }) };
      }
      throw new Error(`the extension must not call ${u}`);
    };

    const result = await ext.handOver({ url: "https://example.com/f.zip", cookie: "sid=SECRET" });

    assert.equal(result.ok, false, "an unproven port must not be treated as the app");
    assert.ok(
      seen.every((u) => !u.endsWith("/add")),
      `no handover may reach an unproven port: ${seen.join(", ")}`,
    );
    assert.equal(state.cancelled.length, 0, "nothing should have been cancelled");
  });

  it("pairs once and then proves every candidate", async () => {
    const state = stubChrome({ cookies: [] });
    const calls = [];
    globalThis.fetch = async (url) => {
      const u = String(url);
      calls.push(u);
      if (u.includes("/ping")) return pingWith(u, TEST_KEY);
      if (u.endsWith("/pair")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ key: TEST_KEY }) };
      }
      if (u.endsWith("/add")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ name: "f.zip" }) };
      }
      throw new Error(`unexpected fetch: ${u}`);
    };

    const result = await ext.handOver({ url: "https://example.com/f.zip" });
    assert.equal(result.ok, true, JSON.stringify(result));
    assert.ok(calls.some((u) => u.endsWith("/pair")), "an unpaired extension must ask for the key");

    // A second handover reuses the stored key: no second /pair.
    const pairsBefore = calls.filter((u) => u.endsWith("/pair")).length;
    await ext.handOver({ url: "https://example.com/g.zip" });
    assert.equal(
      calls.filter((u) => u.endsWith("/pair")).length,
      pairsBefore,
      "the key must be stored, not re-requested on every handover",
    );
  });

  it("treats a refused /pair as 'the app is not running'", async () => {
    stubChrome({ cookies: [{ name: "sid", value: "SECRET" }] });
    const calls = [];
    globalThis.fetch = async (url) => {
      const u = String(url);
      calls.push(u);
      if (u.includes("/ping")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ app: "VKMDownload" }) };
      }
      if (u.endsWith("/pair")) {
        return { ok: false, status: 403, headers: { get: () => null }, json: async () => ({ error: "no" }) };
      }
      throw new Error(`unexpected fetch: ${u}`);
    };

    const result = await ext.handOver({ url: "https://example.com/f.zip" });
    assert.equal(result.ok, false);
    assert.ok(
      calls.every((u) => !u.endsWith("/add")),
      "nothing may be sent to an app that refused to pair",
    );
  });

  it("does not send cookies before a candidate has proven itself", async () => {
    const cookieReads = [];
    stubChrome({ storedKey: TEST_KEY });
    globalThis.chrome.cookies.getAll = async () => {
      cookieReads.push(Date.now());
      return [{ name: "sid", value: "SECRET" }];
    };
    globalThis.fetch = async (url) => {
      const u = String(url);
      if (u.includes("/ping")) {
        return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ app: "VKMDownload" }) };
      }
      throw new Error(`unexpected fetch: ${u}`);
    };

    await ext.handOver({ url: "https://example.com/f.zip", cookie: "" });
    assert.equal(cookieReads.length, 0, "the cookie store must not be touched for an unproven app");
  });

  it("skips other extensions' downloads", () => {
    assert.equal(
      ext.shouldTake({ id: 1, url: "https://example.com/f.zip", byExtensionId: "otherop" }),
      false,
      "another extension's download is not ours to take over",
    );
  });
});
