import { useMemo, useState } from "react";
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
  right?: boolean;
}

/**
 * Columns offered by the sortable header.
 *
 * The order matches --task-cols in styles.css; changing one without the other
 * misaligns the header from the rows it sorts.
 */
const COLUMNS: Column[] = [
  { key: "name", label: "名称" },
  { key: "status", label: "状态" },
  { key: "size", label: "大小 / 添加时间", right: true },
  { key: "progress", label: "进度", right: true },
  { key: "speed", label: "速度 / 分片", right: true },
];

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
              className={`listhead__cell${active ? " listhead__cell--active" : ""}${
                c.right ? " listhead__cell--right" : ""
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
    <div className="listwrap">
      {header()}

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
