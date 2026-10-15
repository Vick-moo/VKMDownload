import { useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";
import { formatBytes, formatWhen } from "../format";
import {
  STATUS_LABEL,
  TYPE_GROUPS,
  fileExt,
  groupOf,
  taskLabel,
} from "../types";
import type { SortDir, SortKey, TaskStatus, TaskView } from "../types";
import { IconArrowDown, IconArrowUp, IconChevron } from "./Icons";
import { TaskRow } from "./TaskRow";

interface Props {
  tasks: TaskView[];
  sortKey: SortKey;
  sortDir: SortDir;
  onSort: (key: SortKey) => void;
  selected: Set<string>;
  onSelect: (id: string, additive: boolean) => void;
  /** Grouping is only meaningful in the unfiltered view. */
  groupByType: boolean;
  onChanged: () => void;
}

interface Column {
  key: SortKey;
  label: string;
  /** Centre the cell contents. Header labels stay left-aligned regardless. */
  center?: boolean;
}

/**
 * Columns offered by the sortable header.
 *
 * The order matches the --task-cols template in styles.css; changing one
 * without the other would misalign the header from the rows it sorts.
 */
const COLUMNS: Column[] = [
  { key: "name", label: "名称" },
  { key: "status", label: "状态", center: true },
  { key: "size", label: "大小 / 添加时间", center: true },
  { key: "progress", label: "进度", center: true },
  { key: "speed", label: "速度 / 分片", center: true },
];

/**
 * Preferred column widths in pixels, in the same order as COLUMNS plus the
 * action track.
 *
 * These are *preferences*, not fixed sizes: when they do not fit the available
 * width every column shrinks in proportion down to MIN_WIDTHS, and when there is
 * room to spare the name column takes it. That is what keeps the list free of a
 * horizontal scrollbar at any window size.
 */
const DEFAULT_WIDTHS = [280, 76, 124, 96, 120, 140];

/**
 * Smallest a column may be. Their total plus the padding and gaps is the
 * narrowest the list can render; below that the columns would overlap, so the
 * window's own minimum width is what actually guarantees this fits.
 */
const MIN_WIDTHS = [140, 62, 104, 78, 96, 112];

/** Where the column widths are remembered between sessions. */
const WIDTHS_KEY = "vkmdownload.column-widths";

/**
 * Reads the column widths saved by a previous session, falling back to the
 * defaults.
 *
 * The stored shape is validated rather than trusted: an entry written by an
 * older build with a different number of columns would otherwise spread the
 * layout into nonsense.
 */
function loadWidths(): number[] {
  try {
    const raw = localStorage.getItem(WIDTHS_KEY);
    if (raw) {
      const parsed: unknown = JSON.parse(raw);
      if (
        Array.isArray(parsed) &&
        parsed.length === DEFAULT_WIDTHS.length &&
        parsed.every(
          (n) => typeof n === "number" && Number.isFinite(n) && n > 0,
        )
      ) {
        return parsed as number[];
      }
    }
  } catch {
    // A malformed entry is not worth failing the list over.
  }
  return DEFAULT_WIDTHS;
}

/** Numeric value of a task for a sortable column. */
function sortValue(t: TaskView, key: SortKey): number {
  switch (key) {
    case "size":
      return t.size;
    case "progress":
      return t.size > 0 ? t.downloaded / t.size : 0;
    case "speed":
      return t.status === "downloading" ? t.speed : -1;
    case "status":
      // Ordered by lifecycle stage, not alphabetically, so "downloading"
      // groups together rather than landing wherever the letter puts it.
      return [
        "failed",
        "downloading",
        "queued",
        "paused",
        "canceled",
        "completed",
      ].indexOf(t.status);
    case "added":
      return new Date(t.createdAt).getTime();
    default:
      return 0;
  }
}

/** Extracts the extension keys present in a set of tasks, for the group header. */
function groupExts(tasks: TaskView[], key: string): string[] {
  const seen = new Set<string>();
  for (const t of tasks) {
    if (groupOf(taskLabel(t)) !== key) continue;
    const e = fileExt(taskLabel(t));
    if (e) seen.add(e);
  }
  return [...seen].sort();
}

/**
 * TaskList renders the sortable header and the tasks beneath it.
 *
 * Tasks are ordered by the active column. The exception is that failures sort
 * to the top of the default view: burying a failed download under a long list
 * is how a user never notices it gave up.
 */
export function TaskList({
  tasks,
  sortKey,
  sortDir,
  onSort,
  selected,
  onSelect,
  groupByType,
  onChanged,
}: Props): JSX.Element {
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});
  // Preferred widths, in the same order as COLUMNS plus the action track.
  // Restored from the last session so a carefully arranged set of columns
  // survives a restart.
  const [prefs, setPrefs] = useState<number[]>(loadWidths);
  const [avail, setAvail] = useState(0);
  const wrapRef = useRef<HTMLDivElement>(null);
  // Kept in a ref so the drag handler reads the current value without being
  // re-created on every render.
  const prefsRef = useRef(prefs);
  prefsRef.current = prefs;

  useEffect(() => {
    try {
      localStorage.setItem(WIDTHS_KEY, JSON.stringify(prefs));
    } catch {
      // Storage can be unavailable; the widths simply will not persist.
    }
  }, [prefs]);

  /**
   * Distributes the available width over the columns' preferred widths.
   *
   * The total never exceeds the container, which is what keeps the list free of
   * a horizontal scrollbar.
   */
  /**
   * Returns the widths to render, in column order.
   *
   * `prefs` is kept balanced against the available width, so in the steady state
   * this returns it unchanged. The two branches below are the exceptions: the
   * window was resized past what the preferences fit, or a drag has pushed the
   * total over the container.
   *
   * Balancing up front is what stops a column jumping when a divider is merely
   * clicked, or when a drag is released. There is no surplus to reallocate at
   * those moments because there is no surplus — and reallocating one was what
   * made the whole row appear to snap back to a default.
   */
  function layoutWidths(preference: number[], available: number): number[] {
    const floor = MIN_WIDTHS.reduce((a, b) => a + b, 0);
    const space = Math.max(available, floor);
    const out = [...preference];
    const total = out.reduce((a, b) => a + b, 0);

    if (total === space) return out;

    if (total < space) {
      // Room to spare: the name column takes it, since it carries the most text
      // and shrinking it is the last thing anyone wants.
      out[0] += space - total;
      return out;
    }

    // Too wide: everything shrinks in proportion, but never below its minimum.
    const need = total - space;
    const shrinkable = out.reduce((sum, w, i) => sum + (w - MIN_WIDTHS[i]), 0);
    if (shrinkable <= 0) return [...MIN_WIDTHS];
    const share = Math.min(need, shrinkable);
    for (let i = 0; i < out.length; i += 1) {
      const room = out[i] - MIN_WIDTHS[i];
      if (room > 0) out[i] -= (share * room) / shrinkable;
    }
    return out;
  }

  // Keep the preferences balanced against the space available, so rendering is a
  // no-op in the steady state. The name column absorbs the difference, for the
  // same reason it takes the surplus above.
  useEffect(() => {
    if (avail <= 0) return;
    setPrefs((prev) => {
      const total = prev.reduce((a, b) => a + b, 0);
      const diff = avail - total;
      if (diff === 0) return prev;
      const next = [...prev];
      next[0] += diff;
      if (next[0] < MIN_WIDTHS[0]) next[0] = MIN_WIDTHS[0];
      return next;
    });
  }, [avail]);

  // Measure the real content box; guessing it from window.innerWidth ignores
  // the sidebar, the scrollbar and the window frame.
  //
  // The columns do not own the whole container: the header and each row carry
  // their own horizontal padding, and the grid adds a gap between tracks. Both
  // are subtracted here, so `avail` is exactly the width the columns may occupy
  // and the last one cannot be pushed past the right edge.
  // Two layers of horizontal inset: the shared one on .listwrap and each row's
  // own. Both are 16px a side, and `clientWidth` includes the first, so the
  // total to subtract is 32px a side.
  const ROW_PADDING = 64;
  /** The gap between columns, matching --space-1. */
  const COL_GAP = 4;
  // Six columns have five boundaries between them, so five gaps — not six.
  const COL_GAPS = 5 * COL_GAP;
  /** Width of the draggable strip straddling a boundary. */
  const HANDLE_WIDTH = 12;

  /**
   * Left offset of the drag handle for the boundary after column `i`.
   *
   * The boundary sits in the middle of the gap that follows that column, and
   * the handle is centred on it. Handles are positioned rather than sized,
   * because a handle as wide as its column covers the header cell and takes the
   * clicks that sort it.
   */
  function handleLeft(i: number): number {
    let acc = 0;
    for (let k = 0; k <= i; k += 1) acc += widths[k];
    return acc + i * COL_GAP + COL_GAP / 2 - HANDLE_WIDTH / 2;
  }

  useEffect(() => {
    const el = wrapRef.current;
    if (!el) return;
    const measure = () => setAvail(el.clientWidth - ROW_PADDING - COL_GAPS);
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    measure();
    return () => ro.disconnect();
  }, []);

  function startColResize(index: number, e: React.MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    const startX = e.clientX;
    // Absolute base for the whole gesture: every move recomputes from here,
    // so applying per-move deltas on top of live state can never accumulate
    // into a runaway (a small drag flying to the end of the screen).
    const startPrefs = [...prefsRef.current];
    let moved = false;

    const onMove = (ev: MouseEvent) => {
      const delta = ev.clientX - startX;

      // A click is not a drag. Without this the layout changed on mousedown and
      // changed back on mouseup, which read as every column snapping to a
      // default when all the user did was click the divider.
      if (!moved) {
        if (Math.abs(delta) < 3) return;
        moved = true;
      }

      // Directional cascade. Divider index sits between column index (left)
      // and index+1 (right); dragging it moves that boundary, so one side
      // grows exactly as much as the other side shrinks:
      // - right: column index grows, the right group (index+1, +2, …) gives,
      //   nearest first;
      // - left: column index+1 grows, the left group (index, -1, …) gives.
      // Columns on the other side are never touched, the total never changes
      // (so the rebalancer stays out of it), and a pinned column simply
      // passes the push further along — no manual pre-moving needed. When
      // nothing can give, the edge stops; returning the previous array skips
      // the render, the persist and the blink with it.
      setPrefs((prev) => {
        let d = delta;
        if (d > 0) {
          let room = 0;
          for (let k = index + 1; k < startPrefs.length; k += 1) {
            room += Math.max(startPrefs[k] - MIN_WIDTHS[k], 0);
          }
          d = Math.min(d, room, Math.max(560 - startPrefs[index], 0));
        } else {
          let room = 0;
          for (let k = index; k >= 0; k -= 1) {
            room += Math.max(startPrefs[k] - MIN_WIDTHS[k], 0);
          }
          d = -Math.min(-d, room, Math.max(560 - startPrefs[index + 1], 0));
        }
        if (d === 0) return prev;
        const next = [...startPrefs];
        if (d > 0) {
          next[index] += d;
          let rem = d;
          for (let k = index + 1; k < next.length && rem > 0; k += 1) {
            const give = Math.max(0, Math.min(next[k] - MIN_WIDTHS[k], rem));
            next[k] -= give;
            rem -= give;
          }
        } else {
          next[index + 1] -= d;
          let rem = -d;
          for (let k = index; k >= 0 && rem > 0; k -= 1) {
            const give = Math.max(0, Math.min(next[k] - MIN_WIDTHS[k], rem));
            next[k] -= give;
            rem -= give;
          }
        }
        return next;
      });
    };
    const onUp = () => {
      document.body.classList.remove("resizing-cols");
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
    };
    document.body.classList.add("resizing-cols");
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  }

  const widths = useMemo(() => layoutWidths(prefs, avail), [prefs, avail]);

  // Every track is an explicit pixel value: a flexible track would fight the
  // proportional shrink above and let the row outgrow the container.
  const template = widths.map((w) => `${w.toFixed(2)}px`).join(" ");




  const sorted = useMemo(() => {
    const dir = sortDir === "asc" ? 1 : -1;
    const copy = [...tasks];
    // Name is textual; everything else is numeric or a timestamp.
    if (sortKey === "name") {
      copy.sort(
        (a, b) =>
          dir *
          a.name.localeCompare(b.name, "zh-Hans-CN", { numeric: true }),
      );
    } else {
      copy.sort((a, b) => dir * (sortValue(a, sortKey) - sortValue(b, sortKey)));
    }

    // Failures float to the top only when the user has not asked for an
    // explicit order, so an explicit sort is never overridden.
    if (sortKey === "added" && sortDir === "desc") {
      copy.sort((a, b) => {
        const af = a.status === "failed" ? 0 : 1;
        const bf = b.status === "failed" ? 0 : 1;
        return af - bf;
      });
    }
    return copy;
  }, [tasks, sortKey, sortDir]);

  const groups = useMemo(() => {
    if (!groupByType) return null;
    return TYPE_GROUPS.map((g) => ({
      group: g,
      items: sorted.filter((t) => groupOf(taskLabel(t)) === g.key),
    })).filter((b) => b.items.length > 0);
  }, [sorted, groupByType]);

  function header() {
    return (
      <div className="listhead" role="row">
        {COLUMNS.map((c) => {
          const active = c.key === sortKey;
          return (
            <button
              key={c.key}
              type="button"
              role="columnheader"
              aria-sort={
                active
                  ? sortDir === "asc"
                    ? "ascending"
                    : "descending"
                  : "none"
              }
              className={`listhead__cell${active ? " listhead__cell--active" : ""}${
                c.center ? " listhead__cell--center" : ""
              }`}
              onClick={() => onSort(c.key)}
            >
              {c.label}
              {active ? (
                <span className="listhead__arrow">
                  {sortDir === "asc" ? (
                    <IconArrowUp size={12} />
                  ) : (
                    <IconArrowDown size={12} />
                  )}
                </span>
              ) : null}
            </button>
          );
        })}
        {/* Placeholder for the action track, which has no header of its own. */}
        <span />
      </div>
    );
  }

  function row(t: TaskView) {
    return (
      <TaskRow
        key={t.id}
        task={t}
        selected={selected.has(t.id)}
        onSelect={onSelect}
        onChanged={onChanged}
      />
    );
  }

  return (
    <div
      className="listwrap"
      ref={wrapRef}
      style={{ "--task-cols": template } as React.CSSProperties}
    >
      {header()}








      {/* Handles sitting on the boundary after each sortable column.
          Absolutely positioned and only a dozen pixels wide. A handle as wide
          as its own column covers the whole header cell, which swallowed the
          clicks that sort — the list could be resized but no longer sorted. */}
      <div className="colresizers" aria-hidden="true">
        {COLUMNS.slice(0, -1).map((c, i) => (
          <span
            key={c.key}
            className="colresizer"
            style={{ left: handleLeft(i) }}
            onMouseDown={(e) => startColResize(i, e)}
            onDoubleClick={() => setPrefs(DEFAULT_WIDTHS)}
            title="拖动调整列宽，双击复位"
          />
        ))}
      </div>

      <div className="list">
        {groups
          ? groups.map(({ group, items }) => {
              const isCollapsed = collapsed[group.key] ?? false;
              const exts = groupExts(items, group.key);
              return (
                <section className="group" key={group.key}>
                  <button
                    type="button"
                    className="group__head"
                    aria-expanded={!isCollapsed}
                    onClick={() =>
                      setCollapsed((prev) => ({
                        ...prev,
                        [group.key]: !isCollapsed,
                      }))
                    }
                  >
                    <span
                      className={`group__chevron${
                        isCollapsed ? " group__chevron--collapsed" : ""
                      }`}
                    >
                      <IconChevron size={14} />
                    </span>
                    <span>{group.label}</span>
                    <span className="group__count">{items.length}</span>
                    {exts.length > 0 ? (
                      <span className="group__count">
                        {exts.slice(0, 6).join(" · ")}
                        {exts.length > 6 ? " …" : ""}
                      </span>
                    ) : null}
                  </button>
                  {!isCollapsed ? (
                    <div className="group__body">{items.map(row)}</div>
                  ) : null}
                </section>
              );
            })
          : sorted.map(row)}
      </div>
    </div>
  );
}
