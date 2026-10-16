import { describe, expect, it } from "vitest";
import { filterLabel } from "./filterLabel";

describe("filterLabel", () => {
  it("names the status filters", () => {
    expect(filterLabel("all")).toBe("全部");
    expect(filterLabel("active")).toBe("下载中");
    expect(filterLabel("completed")).toBe("已完成");
    expect(filterLabel("failed")).toBe("失败");
  });
  it("names extension filters with the extension", () => {
    expect(filterLabel("ext:pdf")).toBe("pdf 文件");
    expect(filterLabel("ext:mp4")).toBe("mp4 文件");
  });
});
