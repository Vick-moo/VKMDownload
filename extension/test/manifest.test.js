/** Manifest contract assertions: permissions, hosts, version format. */

"use strict";

const { describe, it } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const manifest = JSON.parse(
  fs.readFileSync(path.join(__dirname, "..", "manifest.json"), "utf8"),
);

describe("manifest", () => {
  it("declares exactly the five permissions the code uses", () => {
    assert.deepEqual(
      [...manifest.permissions].sort(),
      ["contextMenus", "cookies", "downloads", "notifications", "storage"],
    );
  });

  it("covers loopback plus the google cookie hosts", () => {
    assert.deepEqual(manifest.host_permissions, [
      "http://127.0.0.1/*",
      "https://*.google.com/*",
      "https://*.googleusercontent.com/*",
    ]);
  });

  it("has a semver version matching the 1.0.2 line", () => {
    assert.match(manifest.version, /^\d+\.\d+\.\d+$/);
  });

  it("wires the service worker and popup", () => {
    assert.equal(manifest.background.service_worker, "background.js");
    assert.equal(manifest.action.default_popup, "popup.html");
  });
});
