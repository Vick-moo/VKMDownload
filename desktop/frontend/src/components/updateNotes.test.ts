/**
 * Tests for the update-dialog summary transform: the release body is written
 * for the GitHub page, and the dialog must show only the marked summary, as
 * plain sentences, no matter how long the full body grew.
 */
import { describe, expect, it } from "vitest";
import { toDialogText } from "./UpdateNotesDialog";

describe("toDialogText", () => {
  it("cuts at the marker and keeps only the summary", () => {
    const body = [
      "# VKMDownload 0.2.7",
      "",
      "## 这次更新改了什么",
      "",
      "- **修复 A**",
      "- **修复 B**",
      "",
      "<!-- app-notes-end -->",
      "",
      "## 升级提示",
      "",
      "- 去 chrome://extensions 重载。",
    ].join("\n");

    const got = toDialogText(body);
    expect(got).toContain("修复 A");
    expect(got).toContain("修复 B");
    expect(got).not.toContain("app-notes-end");
    expect(got).not.toContain("升级提示");
    expect(got).not.toContain("chrome://extensions");
  });

  it("strips markdown furniture that only reads well rendered", () => {
    const body = [
      "## 标题",
      "",
      "- **加粗**：`代码` 与 | 表格行 |",
      "-----------------",
    ].join("\n");

    const got = toDialogText(body);
    expect(got).not.toContain("##");
    expect(got).not.toContain("**");
    expect(got).not.toContain("`");
    expect(got).toContain("标题");
    expect(got).toContain("加粗：代码 与");
    // Table rows and rules are dropped entirely: as plain text they are noise.
    expect(got).not.toContain("|");
    expect(got).not.toContain("---");
  });

  it("falls back to the whole body when no marker exists, capped on a line boundary", () => {
    const body = Array.from(
      { length: 80 },
      (_, i) => `第 ${i} 行，这一行有一些内容用来撑长度。`,
    ).join("\n");
    const got = toDialogText(body);
    expect(got.length).toBeLessThanOrEqual(1200);
    // The cut lands on a line boundary: the last line is whole.
    expect(got.endsWith("。")).toBe(true);
  });

  it("falls back to a word-boundary cut for a single over-long line", () => {
    // One line with no newline to cut on. The old fallback sliced at the cap
    // and stopped wherever that landed — the "1200 个 A" case. It now marks
    // the cut, and where there is whitespace to cut on, ends on a whole word.
    const chars = toDialogText("A".repeat(3000));
    expect(chars.length).toBeLessThanOrEqual(1200);
    expect(chars.endsWith("…")).toBe(true);
    expect(chars).not.toBe("A".repeat(1200));

    const words = toDialogText("download ".repeat(200));
    expect(words.length).toBeLessThanOrEqual(1200);
    expect(words.endsWith("download…")).toBe(true);
  });

  it("returns an empty string for an empty body so the caller shows the fallback", () => {
    expect(toDialogText("")).toBe("");
    expect(toDialogText("   \n  ")).toBe("");
  });
});
