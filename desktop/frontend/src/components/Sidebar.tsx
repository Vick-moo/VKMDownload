import { useRef, useState } from "react";
import type { JSX } from "react";
import { formatSpeed } from "../format";
import { TYPE_GROUPS, groupOf } from "../types";
import type { Filter, TaskView } from "../types";
import {
  IconAlert,
  IconCheck,
  IconChevron,
  IconDownload,
  IconInbox,
  IconSettings,
} from "./Icons";

/** Status entries shown in the sidebar. */
const STATUS_NAV: Array<{ key: Filter; label: string; icon: JSX.Element }> = [
  { key: "all", label: "全部", icon: <IconInbox size={18} /> },
  { key: "active", label: "下载中", icon: <IconDownload size={18} /> },
  { key: "completed", label: "已完成", icon: <IconCheck size={18} /> },
  { key: "failed", label: "失败", icon: <IconAlert size={18} /> },
];

interface Props {
  tasks: TaskView[];
  filter: Filter;
  activeCount: number;
  totalSpeed: number;
  settingsOpen: boolean;
  /** Whether the sidebar is collapsed to an icon rail. */
  collapsed: boolean;
  onToggleCollapsed: () => void;
  onFilter: (f: Filter) => void;
  onOpenSettings: () => void;
}

/**
 * Sidebar holds the two ways of narrowing the list, plus the live totals.
 *
 * The type section is collapsible as a whole and each type group collapses on
 * its own, because with a hundred finished downloads the expanded list is
 * longer than the window and the user needs to fold away what they are not
 * looking at.
 */
export function Sidebar({
  tasks,
  filter,
  activeCount,
  totalSpeed,
  settingsOpen,
  collapsed,
  onToggleCollapsed,
  onFilter,
  onOpenSettings,
}: Props): JSX.Element {
  const [typesOpen, setTypesOpen] = useState(true);
  const [subCollapsed, setSubCollapsed] = useState<Record<string, boolean>>({});

  // Resizing is a view preference, so it is kept in state rather than being
  // written to the settings file on every pixel of movement.
  const [width, setWidth] = useState(224);
  const dragging = useRef(false);

  function startResize(e: React.MouseEvent) {
    e.preventDefault();
    dragging.current = true;
    const startX = e.clientX;
    const startW = width;

    const onMove = (ev: MouseEvent) => {
      if (!dragging.current) return;
      // Clamped so the sidebar can never be dragged over the list.
      setWidth(Math.max(180, Math.min(360, startW + ev.clientX - startX)));
    };
    const onUp = () => {
      dragging.current = false;
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
    };
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

  return (
    <nav
      className={`sidebar${collapsed ? " sidebar--collapsed" : ""}`}
      aria-label="任务筛选"
      style={collapsed ? undefined : { width }}
    >
      <div className="sidebar__scroll">
        <button
          type="button"
          className="nav-item nav-item--toggle"
          aria-expanded={!collapsed}
          aria-label={collapsed ? "展开侧栏" : "收起侧栏"}
          title={collapsed ? "展开侧栏" : "收起侧栏"}
          onClick={onToggleCollapsed}
        >
          <span
            className={`sidebar__collapse${collapsed ? " sidebar__collapse--flipped" : ""}`}
          >
            <IconChevron size={14} />
          </span>
          <span className="nav-item__label">{collapsed ? "" : "收起"}</span>
        </button>

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

        {/* A collapsible section of file-type groups. */}
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
              {TYPE_GROUPS.map((g) => {
                const items = tasks.filter((t) => groupOf(t.name) === g.key);
                if (items.length === 0) return null;
                const isCollapsed = subCollapsed[g.key] ?? false;
                const exts = [
                  ...new Set(
                    items
                      .map((t) => {
                        const dot = t.name.lastIndexOf(".");
                        return dot > 0
                          ? t.name.slice(dot + 1).toLowerCase()
                          : "";
                      })
                      .filter(Boolean),
                  ),
                ].sort();

                return (
                  <div className="navsub" key={g.key}>
                    <button
                      type="button"
                      className="navsub__head"
                      aria-expanded={!isCollapsed}
                      onClick={() =>
                        setSubCollapsed((prev) => ({
                          ...prev,
                          [g.key]: !isCollapsed,
                        }))
                      }
                    >
                      <span
                        className={`navsub__chevron${
                          isCollapsed ? " navsub__chevron--collapsed" : ""
                        }`}
                      >
                        <IconChevron size={12} />
                      </span>
                      <span className="navsub__label">{g.label}</span>
                      <span className="nav-item__count">{items.length}</span>
                    </button>
                    {!isCollapsed ? (
                      <div className="navsub__body">
                        {exts.slice(0, 8).map((e) => (
                          <span className="extchip" key={e}>
                            .{e}
                          </span>
                        ))}
                        {exts.length > 8 ? (
                          <span className="extchip">+{exts.length - 8}</span>
                        ) : null}
                      </div>
                    ) : null}
                  </div>
                );
              })}
            </div>
          ) : null}
        </div>
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
        onDoubleClick={() => setWidth(224)}
        title="拖动调整宽度，双击复位"
      />
    </nav>
  );
}
