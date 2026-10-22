import { describe, expect, it } from "vitest";
import {
  formatDiagnosticTime,
  formatLastSeen,
  rejectionReasonLabel,
} from "./PeerDiagnosticsList";

describe("rejectionReasonLabel", () => {
  it("maps every reason the peer guard emits", () => {
    expect(rejectionReasonLabel("bad_addr")).toBe("地址无法解析");
    expect(rejectionReasonLabel("not_loopback")).toBe("不是本机连接");
    expect(rejectionReasonLabel("bad_port")).toBe("端口异常");
    expect(rejectionReasonLabel("no_owner")).toBe("查不到所属进程");
    expect(rejectionReasonLabel("no_image")).toBe("读不到程序文件");
    expect(rejectionReasonLabel("not_allowlisted")).toBe("不是已知浏览器");
    expect(rejectionReasonLabel("not_in_install_root")).toBe("不在安装目录");
    expect(rejectionReasonLabel("bad_signature")).toBe("签名不可信");
    expect(rejectionReasonLabel("origin_locked")).toBe("扩展被锁定");
  });

  it("keeps an unknown reason readable instead of dropping it", () => {
    expect(rejectionReasonLabel("future_reason")).toBe("future_reason");
    expect(rejectionReasonLabel("toString")).toBe("toString");
    expect(rejectionReasonLabel("")).toBe("未知原因");
  });
});

describe("formatLastSeen", () => {
  it("renders missing, zero and unparseable values as 从未", () => {
    expect(formatLastSeen(null)).toBe("从未");
    expect(formatLastSeen(undefined)).toBe("从未");
    expect(formatLastSeen("")).toBe("从未");
    expect(formatLastSeen("0001-01-01T00:00:00Z")).toBe("从未");
    expect(formatLastSeen("not-a-date")).toBe("从未");
    expect(formatLastSeen(0)).toBe("从未");
  });

  it("formats ISO strings, Date objects and Go's time wrapper", () => {
    const iso = "2000-01-02T03:04:05.000Z";
    for (const value of [iso, new Date(iso), { Time: iso }]) {
      const out = formatLastSeen(value);
      expect(out).not.toBe("从未");
      expect(out).toContain("2000");
    }
  });
});

describe("formatDiagnosticTime", () => {
  it("renders junk as an em dash", () => {
    expect(formatDiagnosticTime(null)).toBe("—");
    expect(formatDiagnosticTime("")).toBe("—");
    expect(formatDiagnosticTime("0001-01-01T00:00:00Z")).toBe("—");
    expect(formatDiagnosticTime("not-a-date")).toBe("—");
  });

  it("formats a real timestamp", () => {
    const out = formatDiagnosticTime("2000-01-02T03:04:05.000Z");
    expect(out).not.toBe("—");
    expect(out).toContain("2000");
  });
});
