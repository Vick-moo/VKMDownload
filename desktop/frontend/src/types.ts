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

/**
 * The status views: every filter that is not an extension. Kept separate so a
 * lookup table keyed by status cannot be indexed with an extension filter.
 */
export type StatusFilter = "all" | "active" | "completed" | "failed";

/**
 * Sidebar filter.
 *
 * Either one of the status views, or a file-extension view. The extension form
 * carries the extension itself — "ext:pdf" — so clicking .pdf shows exactly the
 * PDFs, rather than everything a broader group such as 文档 would sweep in.
 */
export type Filter = StatusFilter | `ext:${string}`;

/** Returns the extension a filter selects, or "" for a status filter. */
export function filterExt(filter: Filter): string {
  return filter.startsWith("ext:") ? filter.slice(4) : "";
}

/** Builds the filter that selects one extension. */
export function extFilter(ext: string): Filter {
  return `ext:${ext}`;
}

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

/**
 * Returns the extension of a filename, lower-case and without the dot.
 *
 * It also accepts a URL, because that is all the list has before the server has
 * supplied a name. Without the two guards below a URL would give the wrong
 * answer twice over: a query string would be read as part of the extension
 * (".zip?token=..." → "zip?token=..."), and a dot in the host would win over
 * the one in the file.
 */
export function fileExt(name: string): string {
  let s = name;
  // Drop the query and the fragment.
  const cut = s.search(/[?#]/);
  if (cut >= 0) s = s.slice(0, cut);
  // Work on the last path segment only.
  const slash = s.lastIndexOf("/");
  if (slash >= 0) s = s.slice(slash + 1);

  const dot = s.lastIndexOf(".");
  // A leading dot means a hidden file with no extension (".gitignore").
  if (dot <= 0 || dot === s.length - 1) return "";
  return s.slice(dot + 1).toLowerCase();
}

/** Returns the group key a filename (or a URL, when no name is known) belongs to. */
export function groupOf(name: string): string {
  const ext = fileExt(name);
  if (!ext) return "other";
  for (const g of TYPE_GROUPS) {
    if (g.key !== "other" && g.exts.includes(ext)) return g.key;
  }
  return "other";
}

/**
 * Returns the text to classify and display a task by.
 *
 * A task has no name until the server answers, and a failed one may never get
 * one. Falling back to the URL keeps it in the right type group and gives the
 * row something recognisable to show.
 */
export function taskLabel(task: { name: string; url: string }): string {
  return task.name || task.url;
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
