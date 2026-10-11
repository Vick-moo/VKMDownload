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
 * The lists are deliberately generous. A group is only ever shown when it has
 * files, so an entry costs nothing when unused, while a missing one pushes a
 * perfectly ordinary file into "other" — which is how a .jar ended up there.
 *
 * "other" is a sentinel: it matches anything no other group claimed, so a file
 * with an unexpected extension is still reachable instead of silently
 * disappearing from every filtered view.
 */
export const TYPE_GROUPS: TypeGroup[] = [
  {
    key: "video",
    label: "视频",
    exts: [
      "mp4", "mkv", "avi", "mov", "webm", "flv", "wmv", "m4v", "ts", "mpg",
      "mpeg", "3gp", "3g2", "vob", "ogv", "rmvb", "rm", "m2ts", "mts", "asf",
      "divx", "f4v", "mxf",
    ],
  },
  {
    key: "audio",
    label: "音频",
    exts: [
      "mp3", "flac", "wav", "aac", "ogg", "oga", "m4a", "wma", "opus", "aiff",
      "aif", "alac", "ape", "mid", "midi", "amr", "ac3", "dts", "mka", "wv",
    ],
  },
  {
    key: "image",
    label: "图片",
    exts: [
      "jpg", "jpeg", "jpe", "png", "gif", "webp", "bmp", "svg", "tif", "tiff",
      "avif", "ico", "heic", "heif", "raw", "cr2", "nef", "arw", "dng", "psd",
      "xcf", "jfif",
    ],
  },
  {
    key: "document",
    label: "文档",
    exts: [
      "pdf", "doc", "docx", "docm", "xls", "xlsx", "xlsm", "ppt", "pptx",
      "pptm", "txt", "md", "markdown", "rst", "csv", "tsv", "epub", "mobi",
      "azw3", "rtf", "odt", "ods", "odp", "pages", "numbers", "key", "tex",
      "djvu", "chm", "log", "nfo",
    ],
  },
  {
    key: "archive",
    label: "压缩包",
    exts: [
      "zip", "rar", "7z", "tar", "gz", "tgz", "bz2", "tbz", "tbz2", "xz",
      "txz", "zst", "lz", "lzma", "lzo", "iso", "img", "cab", "arj", "ace",
      "z", "zipx", "jar", "war", "ear", "sar", "aar",
    ],
  },
  {
    key: "program",
    label: "程序",
    exts: [
      "exe", "msi", "msp", "msu", "com", "scr", "dll", "dmg", "pkg", "mpkg",
      "deb", "rpm", "apk", "aab", "ipa", "appx", "msix", "appxbundle", "bat",
      "cmd", "ps1", "psm1", "vbs", "wsf", "snap", "flatpak", "appimage", "run",
      "bin", "elf", "ko", "sys", "drv", "crx", "xpi",
    ],
  },
  {
    key: "code",
    label: "代码",
    exts: [
      "go", "mod", "sum", "js", "mjs", "cjs", "ts", "tsx", "jsx", "py", "pyw",
      "pyi", "ipynb", "java", "class", "kt", "kts", "scala", "groovy", "c",
      "h", "cc", "cpp", "cxx", "hpp", "hh", "hxx", "cs", "rs", "rb", "php",
      "swift", "m", "mm", "dart", "lua", "pl", "pm", "r", "jl", "clj", "cljs",
      "ex", "exs", "erl", "hrl", "hs", "lhs", "ml", "mli", "fs", "fsx", "vb",
      "asm", "s", "f", "f90", "pas", "sql", "sh", "bash", "zsh", "fish", "psm",
      "html", "htm", "xhtml", "css", "scss", "sass", "less", "vue", "svelte",
      "json", "jsonc", "yaml", "yml", "toml", "xml", "xsd", "xsl", "ini",
      "cfg", "conf", "properties", "env", "gradle", "cmake", "mk", "makefile",
      "dockerfile", "proto", "graphql", "gql", "wasm", "wat",
    ],
  },
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
