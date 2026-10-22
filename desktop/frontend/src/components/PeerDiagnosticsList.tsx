import type { JSX } from "react";
import type { main } from "../../wailsjs/go/models";
import { formatWhen } from "../format";

/** Human-readable labels for every reason the peer guard records. */
const REASON_LABELS = new Map<string, string>([
  ["bad_addr", "地址无法解析"],
  ["not_loopback", "不是本机连接"],
  ["bad_port", "端口异常"],
  ["no_owner", "查不到所属进程"],
  ["no_image", "读不到程序文件"],
  ["not_allowlisted", "不是已知浏览器"],
  ["not_in_install_root", "不在安装目录"],
  ["bad_signature", "签名不可信"],
  ["origin_locked", "扩展被锁定"],
]);

/** Maps a wire reason to the sentence shown in the diagnostics list. */
export function rejectionReasonLabel(reason: string): string {
  return REASON_LABELS.get(reason) ?? (reason || "未知原因");
}

/**
 * Extracts an ISO timestamp from the several shapes a Wails time value can
 * arrive in. A time.Time normally serialises as an RFC3339 string, but the
 * generated model types it as `any`, so the Date/struct forms are handled too.
 */
function toISOString(value: unknown): string {
  if (typeof value === "string") return value;
  if (value instanceof Date) return value.toISOString();
  if (value && typeof value === "object") {
    const time = (value as { Time?: unknown }).Time;
    if (time instanceof Date) return time.toISOString();
    if (typeof time === "string") return time;
  }
  return "";
}

/** Returns a usable ISO string, rejecting the Go zero time and junk. */
function usableISO(value: unknown): string {
  const iso = toISOString(value);
  if (!iso || iso.startsWith("0001-01-01T00:00:00")) return "";
  return Number.isNaN(new Date(iso).getTime()) ? "" : iso;
}

/** Formats the extension's last-seen time, or 从未 when it never connected. */
export function formatLastSeen(value: unknown): string {
  const iso = usableISO(value);
  return iso ? formatWhen(iso) : "从未";
}

/** Formats one rejection time; an unreadable value becomes an em dash. */
export function formatDiagnosticTime(value: unknown): string {
  const iso = usableISO(value);
  return iso ? formatWhen(iso) : "—";
}

/**
 * The recent peer-guard refusals, newest first.
 *
 * Only the first 20 are rendered even though the backend already caps its ring:
 * the component stays bounded if that cap ever changes.
 */
export function PeerDiagnosticsList({
  items,
}: {
  items: readonly main.PeerRejection[];
}): JSX.Element {
  if (items.length === 0) {
    return (
      <p className="settings-section__note">没有被拒绝的连接记录。</p>
    );
  }
  return (
    <ul
      style={{
        listStyle: "none",
        margin: 0,
        padding: 0,
        display: "flex",
        flexDirection: "column",
      }}
    >
      {items.slice(0, 20).map((item, i) => (
        <li
          key={`${i}-${item.reason}`}
          style={{
            display: "flex",
            alignItems: "baseline",
            flexWrap: "wrap",
            gap: "var(--space-2)",
            padding: "var(--space-2) 0",
            borderBottom: "1px solid var(--border)",
            fontSize: 12,
            lineHeight: "16px",
          }}
        >
          <span style={{ color: "var(--text-muted)", whiteSpace: "nowrap" }}>
            {formatDiagnosticTime(item.time)}
          </span>
          <span
            title={item.exe || undefined}
            style={{
              maxWidth: 180,
              overflow: "hidden",
              textOverflow: "ellipsis",
              whiteSpace: "nowrap",
              fontFamily: "var(--font-mono)",
              color: "var(--text-secondary)",
            }}
          >
            {item.exe || "—"}
          </span>
          <span
            style={{
              marginLeft: "auto",
              color: "var(--warning)",
              textAlign: "right",
            }}
          >
            {rejectionReasonLabel(item.reason)}
          </span>
        </li>
      ))}
    </ul>
  );
}
