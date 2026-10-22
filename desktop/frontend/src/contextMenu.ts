/**
 * Pure helpers behind the row context menu and the UI state persisted on this
 * machine.
 *
 * They live outside the components so the parts that are easy to get wrong —
 * preserving a multi-selection, keeping a popup inside the window, and falling
 * back from a corrupt localStorage entry — can be tested without a DOM.
 */
import { resumeSkipReason } from "./resumeTargets";
import type { Filter, SortDir, SortKey } from "./types";

/** Keys used for the view state remembered between launches. */
export const STORAGE_KEYS = {
  sort: "vkm.sort",
  filter: "vkm.filter",
  sidebarCollapsed: "vkm.sidebarCollapsed",
} as const;

export interface StoredSort {
  key: SortKey;
  dir: SortDir;
}

/** The list's original order, used when nothing valid is stored. */
export const DEFAULT_SORT: StoredSort = { key: "added", dir: "desc" };

const SORT_KEYS: readonly SortKey[] = [
  "name",
  "status",
  "size",
  "progress",
  "added",
  "speed",
];
const SORT_DIRS: readonly SortDir[] = ["asc", "desc"];
const STATUS_FILTERS: readonly Filter[] = [
  "all",
  "active",
  "completed",
  "failed",
  "canceled",
];

function isSortKey(value: unknown): value is SortKey {
  return (
    typeof value === "string" &&
    (SORT_KEYS as readonly string[]).includes(value)
  );
}

function isSortDir(value: unknown): value is SortDir {
  return (
    typeof value === "string" &&
    (SORT_DIRS as readonly string[]).includes(value)
  );
}

/**
 * Parses the stored sort, falling back to added/desc on anything unexpected:
 * a missing key, an older shape, corrupt JSON, or a field this build no longer
 * has. The renderer must never crash because of a stale localStorage entry.
 */
export function parseStoredSort(raw: string | null): StoredSort {
  if (!raw) return { ...DEFAULT_SORT };
  try {
    const parsed: unknown = JSON.parse(raw);
    if (parsed && typeof parsed === "object") {
      const { key, dir } = parsed as { key?: unknown; dir?: unknown };
      if (isSortKey(key) && isSortDir(dir)) return { key, dir };
    }
  } catch {
    // Corrupt JSON is just an empty preference, not an error to surface.
  }
  return { ...DEFAULT_SORT };
}

/** Parses the stored filter, accepting only the statuses and ext:<name> form. */
export function parseStoredFilter(raw: string | null): Filter {
  if (raw) {
    if ((STATUS_FILTERS as readonly string[]).includes(raw)) {
      return raw as Filter;
    }
    if (/^ext:\S+$/.test(raw)) return raw as Filter;
  }
  return "all";
}

/** Parses the stored collapsed flag; anything but "true" means expanded. */
export function parseStoredSidebarCollapsed(raw: string | null): boolean {
  return raw === "true";
}

/** Reads a localStorage entry, treating unavailable storage as absent. */
export function readStored(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

/** Writes a localStorage entry, ignoring storage that is unavailable/full. */
export function writeStored(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    // Private mode or a full quota: the view simply will not persist.
  }
}

/**
 * Adds a row to the selection for a right-click.
 *
 * Unlike the checkbox's toggle, this never deselects: right-clicking a row that
 * is already selected must leave a multi-selection untouched, which is what
 * lets the context menu act on the row under the cursor without losing the
 * other rows the user picked.
 */
export function selectForContextMenu(
  selected: Set<string>,
  id: string,
): Set<string> {
  if (selected.has(id)) return selected;
  const next = new Set(selected);
  next.add(id);
  return next;
}

/** A viewport-safe position for a fixed-position popup. */
export interface MenuPoint {
  left: number;
  top: number;
}

/**
 * Places the menu at (x, y), then pulls it back inside the viewport.
 *
 * `margin` keeps the popup off the window edge. When the menu is larger than
 * the window itself the margin wins, so the top-left corner stays reachable
 * instead of the menu being pushed off both sides.
 */
export function clampMenuPosition(
  x: number,
  y: number,
  width: number,
  height: number,
  viewportWidth: number,
  viewportHeight: number,
  margin = 8,
): MenuPoint {
  const maxLeft = Math.max(margin, viewportWidth - width - margin);
  const maxTop = Math.max(margin, viewportHeight - height - margin);
  return {
    left: Math.max(margin, Math.min(x, maxLeft)),
    top: Math.max(margin, Math.min(y, maxTop)),
  };
}

/** What the "resume" and "pause" entries should say and allow for a status. */
export interface ContextActions {
  /** "开始" for a queued row, "继续" for the other resumable states. */
  resumeLabel: string;
  canResume: boolean;
  canPause: boolean;
}

/**
 * Mirrors the row's own buttons and Manager.Resume's state gate.
 *
 * A downloading or completed row cannot be resumed, and only a transfer or a
 * queued row can be paused; the menu shows those entries disabled rather than
 * hiding them, so the menu's shape does not jump between rows.
 */
export function contextActions(status: string): ContextActions {
  return {
    resumeLabel: status === "queued" ? "开始" : "继续",
    canResume: resumeSkipReason(status) === null,
    canPause: status === "downloading" || status === "queued",
  };
}
