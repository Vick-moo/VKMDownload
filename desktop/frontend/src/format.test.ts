import { describe, expect, it } from "vitest";
import {
  ellipsizeMiddle,
  formatBytes,
  formatEta,
  formatPercent,
  formatProgress,
  formatSize,
  formatSpeed,
  formatWhen,
  progressParts,
} from "./format";

describe("formatBytes", () => {
  it("renders zero and junk as 0 B", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(-5)).toBe("0 B");
    expect(formatBytes(NaN)).toBe("0 B");
    expect(formatBytes(Infinity)).toBe("0 B");
  });
  it("never renders an undefined unit for a sub-byte value", () => {
    // 0 < bytes < 1 has a negative log, so the exponent floor landed on -1 and
    // units[-1] printed as the literal string "undefined". The clamp keeps it
    // in bytes instead.
    expect(formatBytes(0.5)).toBe("1 B");
    expect(formatBytes(0.1)).not.toContain("undefined");
  });
  it("uses binary units with one decimal below 100", () => {
    expect(formatBytes(1)).toBe("1 B");
    expect(formatBytes(1023)).toBe("1023 B");
    expect(formatBytes(1024)).toBe("1.0 KiB");
    expect(formatBytes(1536)).toBe("1.5 KiB");
    expect(formatBytes(1048576)).toBe("1.0 MiB");
    expect(formatBytes(1073741824)).toBe("1.0 GiB");
  });
  it("rounds to whole numbers at 100 and above", () => {
    expect(formatBytes(100 * 1024)).toBe("100 KiB");
    expect(formatBytes(123456789)).toBe("118 MiB");
  });
});

describe("formatSpeed", () => {
  it("renders zero as 0 B/s", () => {
    expect(formatSpeed(0)).toBe("0 B/s");
    expect(formatSpeed(-1)).toBe("0 B/s");
    expect(formatSpeed(NaN)).toBe("0 B/s");
  });
  it("appends /s to the byte rendering", () => {
    expect(formatSpeed(1536)).toBe("1.5 KiB/s");
    expect(formatSpeed(1048576)).toBe("1.0 MiB/s");
  });
});

describe("formatEta", () => {
  it("renders junk as an em dash", () => {
    expect(formatEta(0)).toBe("—");
    expect(formatEta(-3)).toBe("—");
    expect(formatEta(NaN)).toBe("—");
    expect(formatEta(Infinity)).toBe("—");
  });
  it("renders seconds, minutes, hours", () => {
    expect(formatEta(5)).toBe("5 秒");
    expect(formatEta(59)).toBe("59 秒");
    expect(formatEta(61)).toBe("1 分 01 秒");
    expect(formatEta(3599)).toBe("59 分 59 秒");
    expect(formatEta(3600)).toBe("1 小时 00 分");
    expect(formatEta(7380)).toBe("2 小时 03 分");
  });
});

describe("formatPercent", () => {
  it("renders unknown totals as an em dash", () => {
    expect(formatPercent(5, 0)).toBe("—");
    expect(formatPercent(5, -1)).toBe("—");
    expect(formatPercent(5, NaN)).toBe("—");
  });
  it("clamps to 0-100 with one decimal", () => {
    expect(formatPercent(1, 2)).toBe("50.0%");
    expect(formatPercent(0, 10)).toBe("0.0%");
    expect(formatPercent(10, 10)).toBe("100.0%");
    expect(formatPercent(15, 10)).toBe("100.0%");
    expect(formatPercent(-1, 10)).toBe("0.0%");
  });
});

describe("formatSize", () => {
  it("renders the engine's unknown-size sentinel as 未知", () => {
    expect(formatSize(-1)).toBe("未知");
    expect(formatSize(NaN)).toBe("未知");
  });
  it("treats a zero size as unknown until the task completed", () => {
    // 0 is what a not-yet-probed task carries; only a completed task has a
    // verified zero (an empty file) worth showing as 0 B.
    expect(formatSize(0, "downloading")).toBe("未知");
    expect(formatSize(0, "paused")).toBe("未知");
    expect(formatSize(0, "completed")).toBe("0 B");
  });
  it("formats a known size with binary units", () => {
    expect(formatSize(1536, "downloading")).toBe("1.5 KiB");
    expect(formatSize(1073741824, "completed")).toBe("1.0 GiB");
  });
});

describe("progressParts", () => {
  it("splits a known download into percentage and bytes", () => {
    expect(
      progressParts(194 * 1024 ** 2, 500 * 1024 ** 2),
    ).toEqual({ pct: "38.8%", bytes: "194 MiB" });
  });

  it("uses 未知 for an unknown total", () => {
    expect(progressParts(194 * 1024 ** 2, -1)).toEqual({
      pct: "未知",
      bytes: "194 MiB",
    });
    expect(progressParts(0, 0)).toEqual({ pct: "未知", bytes: "0 B" });
  });

  it("renders a completed task as 100.0%, including an empty file", () => {
    expect(progressParts(0, 0, "completed")).toEqual({
      pct: "100.0%",
      bytes: "0 B",
    });
    expect(progressParts(10, -1, "completed")).toEqual({
      pct: "100.0%",
      bytes: "10 B",
    });
  });
});

describe("formatProgress", () => {
  it("renders percentage and downloaded bytes together", () => {
    expect(formatProgress(0, 10)).toBe("0.0% · 0 B");
    expect(formatProgress(10, 10)).toBe("100.0% · 10 B");
    expect(formatProgress(2 * 1024 ** 3, 4 * 1024 ** 3)).toBe(
      "50.0% · 2.0 GiB",
    );
    expect(formatProgress(3 * 1024 ** 3, 4 * 1024 ** 3)).toBe(
      "75.0% · 3.0 GiB",
    );
  });
  it("says 未知 instead of faking 0% when the total is unknown", () => {
    expect(formatProgress(640 * 1024 * 1024, -1)).toBe("未知 · 640 MiB");
    expect(formatProgress(640 * 1024 * 1024, 0)).toBe("未知 · 640 MiB");
    expect(formatProgress(0, -1)).toBe("未知 · 0 B");
  });
  it("renders a completed task as 100.0%, including an empty file", () => {
    // Same decimal format as an in-flight row, so completion does not make the
    // percentage change shape. A verified 0-byte file is finished even though
    // formatPercent cannot divide by its zero total.
    expect(formatProgress(0, 0, "completed")).toBe("100.0% · 0 B");
    expect(formatProgress(10, -1, "completed")).toBe("100.0% · 10 B");
    expect(formatProgress(10, 10, "completed")).toBe("100.0% · 10 B");
  });
});

describe("formatWhen", () => {
  it("renders junk as an em dash", () => {
    expect(formatWhen("")).toBe("—");
    expect(formatWhen("not-a-date")).toBe("—");
  });
  it("renders recent times relatively", () => {
    const now = Date.now();
    expect(formatWhen(new Date(now - 10_000).toISOString())).toBe("刚刚");
    expect(formatWhen(new Date(now - 5 * 60_000).toISOString())).toBe(
      "5 分钟前",
    );
    expect(formatWhen(new Date(now - 3 * 3_600_000).toISOString())).toBe(
      "3 小时前",
    );
  });
  it("renders old dates with year-month-day", () => {
    expect(formatWhen("2000-01-02T03:04:05.000Z")).toMatch(/^2000-01-02 /);
  });
});

describe("ellipsizeMiddle", () => {
  it("keeps short names whole", () => {
    expect(ellipsizeMiddle("abc.txt")).toBe("abc.txt");
    expect(ellipsizeMiddle("a".repeat(64))).toBe("a".repeat(64));
  });
  it("cuts the middle, keeping head and tail", () => {
    const out = ellipsizeMiddle("a".repeat(100), 64);
    expect(out.length).toBe(64);
    expect(out).toContain("…");
    expect(out.endsWith("a".repeat(Math.ceil(64 / 3)))).toBe(true);
  });
});
