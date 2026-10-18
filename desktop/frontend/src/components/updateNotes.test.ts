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

  it("returns an empty string for an empty body so the caller shows the fallback", () => {
    expect(toDialogText("")).toBe("");
    expect(toDialogText("   \n  ")).toBe("");
  });
});
