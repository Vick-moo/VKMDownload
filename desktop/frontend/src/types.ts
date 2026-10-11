/**
 * Front-end types.
 *
 * Everything that crosses the Go bridge is re-exported from the generated
 * models rather than re-declared, so a change on the Go side becomes a type
 * error here instead of a silent mismatch at runtime.
 */
import type { main } from "../wailsjs/go/models";

export type TaskView = main.TaskView;
export type Config = main.Config;
export type StartRequest = main.StartRequest;

/** Task lifecycle, matching the TaskStatus constants in manager.go. */
export type TaskStatus =
  | "queued"
  | "downloading"
  | "paused"
  | "completed"
  | "failed"
  | "canceled";

/** Sidebar filter, either one of the status views or a file-type group. */
export type Filter = "all" | "active" | "completed" | "failed";

/** Column the task list is ordered by. */
export type SortKey = "name" | "status" | "size" | "progress" | "added" | "speed";

/** Direction of the sort. */
export type SortDir = "asc" | "desc";

/** A file-type group in the sidebar, e.g. 文档 / 压缩包 / 程序. */
export interface TypeGroup {
  key: string;
  label: string;
  /** extensions that belong to this group, lower-case and without the dot */
  exts: string[];
}

/**
 * File-type groups for the collapsible sidebar section.
 *
 * "other" is a sentinel: it matches anything no other group claimed, so a file
 * with an unexpected extension is still reachable instead of silently
 * disappearing from every filtered view.
 */
export const TYPE_GROUPS: TypeGroup[] = [
  { key: "video", label: "视频", exts: ["mp4", "mkv", "avi", "mov", "webm", "flv", "wmv", "m4v", "ts"] },
  { key: "audio", label: "音频", exts: ["mp3", "flac", "wav", "aac", "ogg", "m4a", "wma", "opus"] },
  { key: "image", label: "图片", exts: ["jpg", "jpeg", "png", "gif", "webp", "bmp", "svg", "tif", "tiff", "avif"] },
  { key: "document", label: "文档", exts: ["pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "md", "csv", "epub", "mobi"] },
  { key: "archive", label: "压缩包", exts: ["zip", "rar", "7z", "tar", "gz", "bz2", "xz", "zst", "iso"] },
  { key: "program", label: "程序", exts: ["exe", "msi", "dmg", "pkg", "deb", "rpm", "apk", "appx", "bat", "ps1"] },
  { key: "code", label: "代码", exts: ["go", "js", "ts", "tsx", "jsx", "py", "java", "c", "cpp", "h", "rs", "rb", "php", "json", "yaml", "yml", "xml", "sql", "sh"] },
  { key: "other", label: "其他", exts: [] },
];

/** Returns the extension of a filename, lower-case and without the dot. */
export function fileExt(name: string): string {
  const dot = name.lastIndexOf(".");
  if (dot <= 0 || dot === name.length - 1) return "";
  return name.slice(dot + 1).toLowerCase();
}

/** Returns the group key a filename belongs to. */
export function groupOf(name: string): string {
  const ext = fileExt(name);
  if (!ext) return "other";
  for (const g of TYPE_GROUPS) {
    if (g.key !== "other" && g.exts.includes(ext)) return g.key;
  }
  return "other";
}

/** Human-readable status labels, used by the status badge and the sort. */
export const STATUS_LABEL: Record<TaskStatus, string> = {
  queued: "排队中",
  downloading: "下载中",
  paused: "已暂停",
  completed: "已完成",
  failed: "失败",
  canceled: "已取消",
};

/** Status badge modifier, matching the classes in styles.css. */
export const STATUS_TONE: Record<TaskStatus, string> = {
  queued: "muted",
  downloading: "active",
  paused: "warning",
  completed: "success",
  failed: "danger",
  canceled: "muted",
};
