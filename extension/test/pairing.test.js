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

// ------------------------------------------------------------ port discovery
// Real-machine evidence (Chrome 155): with several loopback probes in flight,
// the app's own answer arrived after its siblings had queued, so a healthy app
// looked missing; the same requests run one after another answered in
// milliseconds. findApp therefore probes strictly serially — do not
// reintroduce a Promise.all fan-out over the port range.

describe("port discovery", () => {
  /**
   * Routes /ping by port and records the scan probes (the ones without a
   * challenge) plus the highest number of fetches that were ever in flight.
   */
  function stubPorts(handler) {
    const probes = [];
    let inFlight = 0;
    let maxInFlight = 0;
    globalThis.fetch = async (url) => {
      const u = String(url);
      const port = Number(new URL(u).port);
      const scan = u.includes("/ping") && !u.includes("challenge=");
      if (scan) probes.push(port);
      inFlight += 1;
      maxInFlight = Math.max(maxInFlight, inFlight);
      try {
        await new Promise((resolve) => setTimeout(resolve, 5));
        if (u.includes("/ping")) return handler(port, u);
        if (u.endsWith("/pair")) {
          return { ok: false, status: 403, headers: { get: () => null }, json: async () => ({ error: "no" }) };
        }
        throw new Error(`unexpected fetch: ${u}`);
      } finally {
        inFlight -= 1;
      }
    };
    return { probes, maxInFlight: () => maxInFlight };
  }

  /** An impostor answer: the right body, no proof header. */
  function impostor() {
    return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ app: "VKMDownload" }) };
  }

  it("stops after 18888 when it is the app", async () => {
    const urls = [];
    stubChrome({ storedKey: TEST_KEY });
    globalThis.fetch = async (url) => {
      const u = String(url);
      urls.push(u);
      if (u.includes("/ping")) return pingWith(u, TEST_KEY);
      throw new Error(`no other request expected: ${u}`);
    };

    const base = await ext.findApp();
    assert.equal(base, "http://127.0.0.1:18888");
    assert.ok(
      urls.every((u) => u.startsWith("http://127.0.0.1:18888/")),
      `18888 answering must not trigger a scan of the range: ${urls.join(", ")}`,
    );
    assert.equal(new Set(urls.map((u) => new URL(u).port)).size, 1, "only one port may be contacted");
  });

  it("probes every port in order, one request at a time", async () => {
    const ports = stubPorts((port) => {
      // 18888 is empty; 18889-18898 answer with the right body but cannot prove
      // the key.
      if (port === 18888) {
        return { ok: false, status: 404, headers: { get: () => null }, json: async () => ({}) };
      }
      return impostor();
    });
    stubChrome({ storedKey: TEST_KEY });

    const base = await ext.findApp();
    assert.equal(base, null, "an unproven port must not be treated as the app");

    assert.deepEqual(
      ports.probes,
      [18888, 18889, 18890, 18891, 18892, 18893, 18894, 18895, 18896, 18897, 18898],
      "the whole range must be probed once, in ascending order",
    );
    assert.equal(ports.maxInFlight(), 1, "probes must be strictly serial, never overlapping");
  });

  it("a failed probe does not stop the ports after it", async () => {
    const ports = stubPorts((port, u) => {
      if (port === 18888) throw new Error("ECONNREFUSED");
      if (port === 18890) throw new Error("timeout");
      if (port === 18893) return pingWith(u, TEST_KEY);
      return impostor();
    });
    stubChrome({ storedKey: TEST_KEY });

    const base = await ext.findApp();
    assert.equal(base, "http://127.0.0.1:18893", "the real app must be found past the dead ports");
    assert.ok(
      !ports.probes.includes(18894),
      `scanning must stop once a port proves itself, probed: ${ports.probes.join(",")}`,
    );
    assert.equal(ports.maxInFlight(), 1);
  });

  it("returns null when every port fails", async () => {
    stubChrome({});
    globalThis.fetch = async () => {
      throw new Error("ECONNREFUSED");
    };

    assert.equal(await ext.findApp(), null);
  });
});
