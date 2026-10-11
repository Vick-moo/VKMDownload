import { useRef, useState } from "react";
import type { JSX } from "react";
import { formatSpeed } from "../format";
import { extFilter } from "../types";
import type { Filter, StatusFilter, TaskView } from "../types";
import {
  IconAlert,
  IconCheck,
  IconChevron,
  IconDownload,
  IconInbox,
  IconSettings,
} from "./Icons";

/** Status entries shown in the sidebar. */
const STATUS_NAV: Array<{
  key: StatusFilter;
  label: string;
  icon: JSX.Element;
}> = [
  { key: "all", label: "全部", icon: <IconInbox size={18} /> },
  { key: "active", label: "下载中", icon: <IconDownload size={18} /> },
  { key: "completed", label: "已完成", icon: <IconCheck size={18} /> },
  { key: "failed", label: "失败", icon: <IconAlert size={18} /> },
];

interface Props {
  tasks: TaskView[];
  filter: Filter;
  /** Every extension present in the list, with how many tasks carry it. */
  extCounts: Array<{ ext: string; count: number }>;
  activeCount: number;
  totalSpeed: number;
  settingsOpen: boolean;
  /** Whether the sidebar is collapsed to an icon rail. */
  collapsed: boolean;
  /** Width in pixels, owned by the parent because the grid column uses it. */
  width: number;
  onWidthChange: (w: number) => void;
  onToggleCollapsed: () => void;
  onFilter: (f: Filter) => void;
  onOpenSettings: () => void;
}

/**
 * Sidebar holds the two ways of narrowing the list, plus the live totals.
 *
 * The type section lists one entry per extension actually present — .zip, .pdf,
 * .png — and each one filters the list to exactly those files. Grouping them
 * into broader categories such as 文档 looked tidier but was less useful: the
 * user who wants their PDFs does not want the spreadsheets too.
 */
export function Sidebar({
  tasks,
  filter,
  extCounts,
  activeCount,
  totalSpeed,
  settingsOpen,
  collapsed,
  width,
  onWidthChange,
  onToggleCollapsed,
  onFilter,
  onOpenSettings,
}: Props): JSX.Element {
  const [typesOpen, setTypesOpen] = useState(true);

  /** Width the owner had when a drag started, and where the pointer was. */
  const dragRef = useRef({ startX: 0, startW: 0, frame: 0 });

  /**
   * Resizes the sidebar by dragging its right edge.
   *
   * Updates are coalesced into an animation frame: a mousemove can fire far more
   * often than the screen refreshes, and re-rendering the whole task list on
   * every one of them is what made the drag feel like it was fighting back.
   */
  function startResize(e: React.MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    dragRef.current.startX = e.clientX;
    dragRef.current.startW = width;

    const onMove = (ev: MouseEvent) => {
      const { startX, startW } = dragRef.current;
      const next = Math.max(180, Math.min(420, startW + ev.clientX - startX));
      if (dragRef.current.frame) cancelAnimationFrame(dragRef.current.frame);
      dragRef.current.frame = requestAnimationFrame(() =>
        onWidthChange(next),
      );
    };
    const onUp = () => {
      if (dragRef.current.frame) cancelAnimationFrame(dragRef.current.frame);
      dragRef.current.frame = 0;
      document.body.classList.remove("resizing");
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
    };
    // A class on the body keeps the cursor consistent even when the pointer
    // outruns the 6px handle.
    document.body.classList.add("resizing");
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  }

  const counts = {
    all: tasks.length,
    active: tasks.filter(
      (t) => t.status === "downloading" || t.status === "queued",
    ).length,
    completed: tasks.filter((t) => t.status === "completed").length,
    failed: tasks.filter((t) => t.status === "failed").length,
  };

  // The extension list is built by the parent and passed in: it is derived from
  // the same tasks the list is rendered from, so computing it twice would be
  // two chances to disagree.

  return (
    <nav
      className={`sidebar${collapsed ? " sidebar--collapsed" : ""}`}
      aria-label="任务筛选"
    >
      {/* Brand and the collapse control share one row. The brand moved here
          from a top bar, which was spending a whole strip of vertical space on
          a label and a version number. */}
      <div className="sidebar__head">
        {collapsed ? (
          // In the rail there is no room for a separate chevron, and the logo is
          // the obvious thing to click, so it becomes the control itself.
          <button
            type="button"
            className="sidebar__logo sidebar__logo--button"
            aria-expanded={false}
            aria-label="展开侧栏"
            title="展开侧栏"
            onClick={onToggleCollapsed}
          >
            <IconDownload size={14} />
          </button>
        ) : (
          <>
            <span className="sidebar__brand">
              <span className="sidebar__logo">
                <IconDownload size={14} />
              </span>
              <span className="sidebar__brand-name">VDownload</span>
            </span>

            <button
              type="button"
              className="sidebar__toggle"
              aria-expanded
              aria-label="收起侧栏"
              title="收起侧栏"
              onClick={onToggleCollapsed}
            >
              <IconChevron size={14} className="chevron--left" />
            </button>
          </>
        )}
      </div>

      <div className="sidebar__scroll">
        {STATUS_NAV.map((item) => {
          const active = !settingsOpen && filter === item.key;
          return (
            <button
              key={item.key}
              type="button"
              className={`nav-item${active ? " nav-item--active" : ""}`}
              aria-current={active ? "page" : undefined}
              title={item.label}
              onClick={() => onFilter(item.key)}
            >
              <span className="nav-item__icon">{item.icon}</span>
              <span className="nav-item__label">{item.label}</span>
              <span className="nav-item__count">{counts[item.key]}</span>
            </button>
          );
        })}

        {/* A collapsible section of file-type groups, built from whatever the
            tasks actually are. It is not rendered at all when no task has a
            classifiable extension. */}
        {/* One entry per extension actually present, each of which narrows the
            list to exactly those files. The whole section is hidden when no
            task has an extension to offer. */}
        {extCounts.length > 0 ? (
          <div className="navgroup">
            <button
              type="button"
              className="navgroup__head"
              aria-expanded={typesOpen}
              onClick={() => setTypesOpen((v) => !v)}
            >
              <span
                className={`navgroup__chevron${
                  typesOpen ? "" : " navgroup__chevron--collapsed"
                }`}
              >
                <IconChevron size={14} />
              </span>
              <span className="navgroup__title">按类型</span>
            </button>

            {typesOpen ? (
              <div className="navgroup__body">
                {extCounts.map(({ ext, count }) => {
                  const key = extFilter(ext);
                  const active = filter === key;
                  return (
                    <button
                      key={ext}
                      type="button"
                      className={`nav-item nav-item--compact${
                        active ? " nav-item--active" : ""
                      }`}
                      aria-current={active ? "page" : undefined}
                      title={`只看 .${ext} 文件`}
                      onClick={() => onFilter(key)}
                    >
                      <span className="nav-item__label">.{ext}</span>
                      <span className="nav-item__count">{count}</span>
                    </button>
                  );
                })}
              </div>
            ) : null}
          </div>
        ) : null}
      </div>

      <button
        type="button"
        className={`nav-item${settingsOpen ? " nav-item--active" : ""}`}
        aria-current={settingsOpen ? "page" : undefined}
        onClick={onOpenSettings}
      >
        <span className="nav-item__icon">
          <IconSettings size={18} />
        </span>
        <span className="nav-item__label">设置</span>
      </button>

      <div className="sidebar__stats">
        <div className="sidebar__stat">
          <span className="sidebar__stat-label">活动任务</span>
          <span className="sidebar__stat-value num">{activeCount}</span>
        </div>
        <div className="sidebar__stat">
          <span className="sidebar__stat-label">总速度</span>
          <span className="sidebar__stat-value num">
            {formatSpeed(totalSpeed)}
          </span>
        </div>
      </div>

      <div
        className="sidebar__resizer"
        role="separator"
        aria-orientation="vertical"
        aria-label="调整侧栏宽度"
        onMouseDown={startResize}
        onDoubleClick={() => onWidthChange(224)}
        title="拖动调整宽度，双击复位"
      />
    </nav>
  );
}
