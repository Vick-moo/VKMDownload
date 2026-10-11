import { useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";
import { formatBytes, formatWhen } from "../format";
import {
  STATUS_LABEL,
  TYPE_GROUPS,
  fileExt,
  groupOf,
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
const DEFAULT_WIDTHS = [280, 76, 104, 96, 120, 140];

/**
 * Smallest a column may be. Their total plus the padding and gaps is the
 * narrowest the list can render; below that the columns would overlap, so the
 * window's own minimum width is what actually guarantees this fits.
 */
const MIN_WIDTHS = [140, 62, 84, 78, 96, 112];

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
    if (groupOf(t.name) !== key) continue;
    const e = fileExt(t.name);
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
  const [prefs, setPrefs] = useState<number[]>(DEFAULT_WIDTHS);
  const [avail, setAvail] = useState(0);
  /** Column currently being dragged, or -1. It keeps its width during layout. */
  const [priority, setPriority] = useState(-1);
  const wrapRef = useRef<HTMLDivElement>(null);
  // Kept in refs so the drag handler reads current values without being
  // re-created on every render.
  const prefsRef = useRef(prefs);
  const availRef = useRef(avail);
  prefsRef.current = prefs;
  availRef.current = avail;

  /**
   * Distributes the available width over the columns' preferred widths.
   *
   * `priority` is the column the user is dragging: it keeps the width asked for,
   * and the others absorb the difference in proportion to their preferences.
   * Without that the dragged column gives back exactly what it gained — the
   * shrink below takes from every column, including the one being resized — and
   * the drag appears to do nothing at all.
   *
   * The total never exceeds the container, which is what keeps the list free of
   * a horizontal scrollbar.
   */
  function layoutWidths(
    preference: number[],
    available: number,
    priority = -1,
  ): number[] {
    const floor = MIN_WIDTHS.reduce((a, b) => a + b, 0);
    const space = Math.max(available, floor);
    const total = preference.reduce((a, b) => a + b, 0);

    if (total <= space) {
      // Give any surplus to the name column; it carries the most text.
      const out = [...preference];
      out[0] += space - total;
      return out;
    }

    // Too wide: shrink, but never below the minimum.
    const out = [...preference];
    const take = total - space;

    if (priority >= 0) {
      // Spread the reduction over everything except the column being dragged.
      let others = 0;
      for (let i = 0; i < out.length; i += 1) {
        if (i !== priority) others += out[i] - MIN_WIDTHS[i];
      }
      if (others <= 0) return [...MIN_WIDTHS];
      for (let i = 0; i < out.length; i += 1) {
        if (i === priority) continue;
        const room = out[i] - MIN_WIDTHS[i];
        if (room > 0) out[i] -= (take * room) / others;
      }
      return out;
    }

    const shrinkable = out.reduce((sum, w, i) => sum + (w - MIN_WIDTHS[i]), 0);
    if (shrinkable <= 0) return [...MIN_WIDTHS];
    const share = Math.min(take, shrinkable);
    for (let i = 0; i < out.length; i += 1) {
      const room = out[i] - MIN_WIDTHS[i];
      if (room > 0) out[i] -= (share * room) / shrinkable;
    }
    return out;
  }

  // Measure the real content box; guessing it from window.innerWidth ignores
  // the sidebar, the scrollbar and the window frame.
  //
  // The columns do not own the whole container: the header and each row carry
  // their own horizontal padding, and the grid adds a gap between tracks. Both
  // are subtracted here, so `avail` is exactly the width the columns may occupy
  // and the last one cannot be pushed past the right edge.
  const ROW_PADDING = 32; // 16px each side, from .listhead and .task
  const COL_GAPS = 6 * 6; // one gap per column boundary
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
    const startPref = prefsRef.current[index];

    // Mark which column is being resized so the layout keeps its width and
    // takes the difference from the others.
    setPriority(index);

    const onMove = (ev: MouseEvent) => {
      // Driven by the pointer position rather than an accumulated delta: the
      // column edge follows the cursor exactly, which is what makes the drag
      // feel predictable.
      const wanted = Math.max(
        MIN_WIDTHS[index],
        Math.min(560, startPref + (ev.clientX - startX)),
      );
      setPrefs(() => {
        const next = [...prefsRef.current];
        next[index] = wanted;
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

  const widths = useMemo(
    () => layoutWidths(prefs, avail, priority),
    [prefs, avail, priority],
  );

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
      items: sorted.filter((t) => groupOf(t.name) === g.key),
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
              className={`listhead__cell${active ? " listhead__cell--active" : ""}`}
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




      {/* Handles sitting on the boundary after each sortable column. They are
          siblings of the header cells rather than children: a button cannot
          contain another interactive element, and the cell already handles the
          click that sorts. */}
      <div className="colresizers" aria-hidden="true">
        {COLUMNS.map((c, i) =>
          i === COLUMNS.length - 1 ? null : (
            <span
              key={c.key}
              className="colresizer"
              style={{ width: widths[i] }}
              onMouseDown={(e) => startColResize(i, e)}
              onDoubleClick={() => { setPrefs(DEFAULT_WIDTHS); setPriority(-1); }}
              title="拖动调整列宽，双击复位"
            />
          ),
        )}
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
