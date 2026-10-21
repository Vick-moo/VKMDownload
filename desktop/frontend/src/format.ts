/**
 * Value formatters shared by the task list and the dialogs.
 *
 * They live together so a byte count is never rendered two different ways in
 * two different places, which is the usual way a UI starts looking sloppy.
 */
import type { TaskStatus } from "./types";

/** Formats a byte count with binary units. */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  // The floor applies to both ends: a value in (0, 1) has a negative log, and
  // Math.floor would land on -1 — units[-1] is undefined, which used to render
  // literally as "undefined KiB". Clamping to 0 keeps such a value in bytes.
  const exp = Math.max(
    0,
    Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1),
  );
  const value = bytes / 1024 ** exp;
  return `${value >= 100 || exp === 0 ? Math.round(value) : value.toFixed(1)} ${units[exp]}`;
}

/** Formats a byte/sec rate. Zero renders as "0 B/s" so a stat keeps its width
 * instead of twitching between a dash and a number. */
export function formatSpeed(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return "0 B/s";
  return `${formatBytes(bytesPerSecond)}/s`;
}

/** Formats a count of seconds as a compact remaining time. */
export function formatEta(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "—";
  const s = Math.round(seconds);
  if (s < 60) return `${s} 秒`;
  if (s < 3600) {
    return `${Math.floor(s / 60)} 分 ${String(s % 60).padStart(2, "0")} 秒`;
  }
  return `${Math.floor(s / 3600)} 小时 ${String(Math.floor((s % 3600) / 60)).padStart(2, "0")} 分`;
}

/** Formats a percentage, guarding against an unknown total. */
export function formatPercent(done: number, total: number): string {
  if (!Number.isFinite(total) || total <= 0) return "—";
  const pct = (done / total) * 100;
  return `${Math.max(0, Math.min(100, pct)).toFixed(1)}%`;
}

/**
 * Formats a task's total size.
 *
 * The engine reports -1 when the server never said how large the file is (a
 * chunked response, or a HEAD the host refused), and a task that has not been
 * probed yet carries 0. Both mean "unknown", not "0 B": rendering them as zero
 * made an in-flight download look empty. A completed task is the exception —
 * there 0 is a real, verified size.
 */
export function formatSize(bytes: number, status?: TaskStatus): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "未知";
  if (bytes === 0 && status !== "completed") return "未知";
  return formatBytes(bytes);
}

/**
 * Formats the progress cell: percentage and downloaded bytes together.
 *
 * When the total is unknown the percentage becomes "未知" instead of a fake
 * 0%; the downloaded count is real either way and stays visible.
 */
export function formatProgress(done: number, total: number): string {
  const pct = formatPercent(done, total);
  return `${pct === "—" ? "未知" : pct} · ${formatBytes(done)}`;
}

/** Cached "today": the stamp is compared per rendered row, and rebuilding two
 * Date objects per row per tick was pure allocation churn on large lists. */
let todayString = "";
let todayStamp = 0;
function todayLabel(): string {
  const now = Date.now();
  if (todayString === "" || now - todayStamp > 60_000) {
    todayStamp = now;
    todayString = new Date().toDateString();
  }
  return todayString;
}

/** Formats an RFC3339 timestamp as a local, compact date-time. */
export function formatWhen(iso: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  const now = Date.now();
  const diff = now - d.getTime();

  if (diff >= 0 && diff < 60_000) return "刚刚";
  if (diff >= 0 && diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  if (diff >= 0 && diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`;

  const pad = (n: number) => String(n).padStart(2, "0");
  const ymd = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  // Today's date is redundant, so only the time is shown for recent items.
  if (d.toDateString() === todayLabel()) {
    return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }
  return `${ymd} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/**
 * Shortens a long file name from the middle.
 *
 * The tail is kept because the extension carries more information than the
 * middle of a hash-like name, and the head is kept so the file is still
 * recognisable.
 */
export function ellipsizeMiddle(name: string, max = 64): string {
  if (name.length <= max) return name;
  const keepEnd = Math.ceil(max / 3);
  const keepStart = max - keepEnd - 1;
  return `${name.slice(0, keepStart)}…${name.slice(name.length - keepEnd)}`;
}
