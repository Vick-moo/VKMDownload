import { useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";
import { DEFAULT_WIDTHS, MIN_WIDTHS, layoutWidths } from "../columns";
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
  /** Reports a failed row action to the toolbar notice line. */
  onError: (message: string) => void;
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

/** Collator for the name column. One shared instance: building an ICU
 * collator is expensive enough that doing it per comparison dwarfed the
 * actual sort. */
const nameCollator = new Intl.Collator("zh-Hans-CN", { numeric: true });

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

/**
 * Sorts the list once per change instead of doing real work per comparison.
 *
 * The failed-pin used to be a second, stable re-sort pass after the main one;
 * folding it in as the primary comparison term gives the same order (rows
 * with equal pin fall through to the column compare) for half the sort calls.
 */
function sortTasks(tasks: TaskView[], key: SortKey, dir: number): TaskView[] {
  const pinned = key === "added" && dir === -1;
  const pin = (t: TaskView) => (pinned && t.status === "failed" ? 0 : 1);
  const copy = [...tasks];
  if (key === "name") {
    // Name is textual; everything else is numeric or a timestamp.
    copy.sort(
      (a, b) =>
        pin(a) - pin(b) || dir * nameCollator.compare(a.name, b.name),
    );
    return copy;
  }
  // Precompute each row's key: the old comparator re-derived it (a Date parse,
  // a division, an indexOf) for every one of the O(n log n) pairs.
  const keys = new Map<string, number>();
  for (const t of tasks) keys.set(t.id, sortValue(t, key));
  copy.sort(
    (a, b) =>
      pin(a) - pin(b) ||
      dir * ((keys.get(a.id) as number) - (keys.get(b.id) as number)),
  );
  return copy;
}

/**
 * Cheap fingerprint of everything the active sort reads. A progress tick only
 * moves downloaded/speed on rows whose position does not depend on them, so
 * an unchanged fingerprint means the previous order still holds — and the
 * re-sort (localeCompare over hundreds of rows, four times a second) can be
 * skipped entirely.
 */
function orderFingerprint(tasks: TaskView[], key: SortKey): string | null {
  if (key === "speed" || key === "progress") {
    // These sorts read fields that change on every tick; no shortcut applies.
    return null;
  }
  const parts: string[] = [];
  for (const t of tasks) {
    parts.push(
      t.id,
      t.status,
      key === "name"
        ? t.name
        : key === "added"
          ? t.createdAt
          : key === "size"
            ? String(t.size)
            : "",
    );
  }
  return parts.join("\u0000");
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
  onError,
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

  // Debounced persistence: the drag handler updates prefs on every mousemove,
  // and a synchronous JSON stringify + write per frame made resizing columns
  // feel exactly as heavy as it sounded.
  useEffect(() => {
    const id = window.setTimeout(() => {
      try {
        localStorage.setItem(WIDTHS_KEY, JSON.stringify(prefs));
      } catch {
        // Storage can be unavailable; the widths simply will not persist.
      }
    }, 300);
    return () => window.clearTimeout(id);
  }, [prefs]);

  // Unmount flush for the debounced write above: switching a filter or
  // opening settings unmounts this list, and the pending 300ms timer would
  // die with it — taking the whole drag gesture's worth of width changes
  // along. prefsRef (kept for the drag handler) already mirrors the latest
  // widths, so the flush always writes the current value.
  useEffect(
    () => () => {
      try {
        localStorage.setItem(WIDTHS_KEY, JSON.stringify(prefsRef.current));
      } catch {
        // Storage can be unavailable; the widths simply will not persist.
      }
    },
    [],
  );

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
   * Widest a single column may grow while dragging, as a share of the
   * container. It used to be a fixed 560px, which stopped making sense on
   * very narrow or very wide windows; a share scales with both.
   */
  const MAX_COL_SHARE = 0.6;
  /** Floor used before the first measure, when `avail` is still 0. */
  const MAX_COL_FALLBACK = 560;
  // Mirrors `avail` for the drag handler below, which runs outside renders.
  const availRef = useRef(avail);
  availRef.current = avail;

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
        const cap =
          availRef.current > 0
            ? Math.max(320, Math.floor(availRef.current * MAX_COL_SHARE))
            : MAX_COL_FALLBACK;
        if (d > 0) {
          let room = 0;
          for (let k = index + 1; k < startPrefs.length; k += 1) {
            room += Math.max(startPrefs[k] - MIN_WIDTHS[k], 0);
          }
          d = Math.min(d, room, Math.max(cap - startPrefs[index], 0));
        } else {
          let room = 0;
          for (let k = index; k >= 0; k -= 1) {
            room += Math.max(startPrefs[k] - MIN_WIDTHS[k], 0);
          }
          d = -Math.min(-d, room, Math.max(cap - startPrefs[index + 1], 0));
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




  // The previous sort result, reused when a tick cannot have changed the
  // order (see orderFingerprint).
  const sortedRef = useRef<{
    key: SortKey;
    dir: SortDir;
    fp: string;
    ids: string[];
  } | null>(null);

  const sorted = useMemo(() => {
    const dir = sortDir === "asc" ? 1 : -1;
    // Failures float to the top only when the user has not asked for an
    // explicit order, so an explicit sort is never overridden (see
    // sortTasks).
    const fp = orderFingerprint(tasks, sortKey);
    const prev = sortedRef.current;
    if (
      fp !== null &&
      prev !== null &&
      prev.key === sortKey &&
      prev.dir === sortDir &&
      prev.fp === fp
    ) {
      // Same rows and the same order-relevant fields: map the remembered
      // order onto the fresh objects (so ticked rows still re-render) and
      // skip the sort entirely.
      const byId = new Map(tasks.map((t) => [t.id, t]));
      const reused = prev.ids
        .map((id) => byId.get(id))
        .filter((t): t is TaskView => t !== undefined);
      if (reused.length === tasks.length) {
        return reused;
      }
    }
    const result = sortTasks(tasks, sortKey, dir);
    sortedRef.current =
      fp === null
        ? null
        : { key: sortKey, dir: sortDir, fp, ids: result.map((t) => t.id) };
    return result;
  }, [tasks, sortKey, sortDir]);

  const groups = useMemo(() => {
    if (!groupByType) return null;
    // One pass builds every bucket and its header extension list together;
    // the old version re-scanned the whole list once per group.
    const buckets = new Map(
      TYPE_GROUPS.map((g) => [
        g.key,
        { group: g, items: [] as TaskView[], exts: new Set<string>() },
      ]),
    );
    for (const t of sorted) {
      const label = taskLabel(t);
      const b = buckets.get(groupOf(label));
      if (!b) continue;
      b.items.push(t);
      const e = fileExt(label);
      if (e) b.exts.add(e);
    }
    return TYPE_GROUPS.map((g) => buckets.get(g.key)!)
      .filter((b) => b.items.length > 0)
      .map((b) => ({ group: b.group, items: b.items, exts: [...b.exts].sort() }));
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
        onError={onError}
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
          ? groups.map(({ group, items, exts }) => {
              const isCollapsed = collapsed[group.key] ?? false;
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
