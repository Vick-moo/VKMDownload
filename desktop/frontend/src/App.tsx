import { useCallback, useEffect, useMemo, useState } from "react";
import type { JSX } from "react";
import { Version } from "../wailsjs/go/main/App";
import {
  IconAlert,
  IconCheck,
  IconDownload,
  IconFolder,
  IconInbox,
  IconPlus,
  IconSettings,
} from "./components/Icons";
import type { Config, Filter, TaskView } from "./types";

/**
 * Navigation entries. The counts come from the task list once it exists; they
 * are rendered now so the layout is settled rather than shifting later.
 */
const NAV: Array<{
  key: Filter | "settings";
  label: string;
  icon: JSX.Element;
}> = [
  { key: "all", label: "全部", icon: <IconInbox size={18} /> },
  { key: "active", label: "下载中", icon: <IconDownload size={18} /> },
  { key: "completed", label: "已完成", icon: <IconCheck size={18} /> },
  { key: "failed", label: "失败", icon: <IconAlert size={18} /> },
];

/** Formats a byte count with binary units. */
function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  const exp = Math.min(
    Math.floor(Math.log(bytes) / Math.log(1024)),
    units.length - 1,
  );
  const value = bytes / 1024 ** exp;
  return `${value >= 100 || exp === 0 ? Math.round(value) : value.toFixed(1)} ${units[exp]}`;
}

/** Formats a byte/sec rate. Zero renders as "0 B/s" rather than a dash so the
 * top-bar stat keeps a constant width instead of twitching as transfers start
 * and stop. */
function formatSpeed(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return "0 B/s";
  return `${formatBytes(bytesPerSecond)}/s`;
}

/** Formats a count of seconds as a compact remaining-time string. */
function formatEta(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "—";
  const s = Math.round(seconds);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m${String(s % 60).padStart(2, "0")}s`;
  return `${Math.floor(s / 3600)}h${String(Math.floor((s % 3600) / 60)).padStart(2, "0")}m`;
}

function App(): JSX.Element {
  const [filter, setFilter] = useState<Filter>("all");
  const [showSettings, setShowSettings] = useState(false);
  const [tasks, setTasks] = useState<TaskView[]>([]);
  const [config, setConfig] = useState<Config | null>(null);
  const [version, setVersion] = useState("");

  // Proves the Go bridge works; replaced by real data in the next step.
  useEffect(() => {
    Version()
      .then(setVersion)
      .catch(() => setVersion(""));
  }, []);

  const counts = useMemo(() => {
    const active = tasks.filter((t) => t.status === "downloading").length;
    const completed = tasks.filter((t) => t.status === "completed").length;
    const failed = tasks.filter((t) => t.status === "failed").length;
    return { all: tasks.length, active, completed, failed };
  }, [tasks]);

  const stats = useMemo(() => {
    const active = tasks.filter((t) => t.status === "downloading");
    return {
      activeCount: active.length,
      totalSpeed: active.reduce((sum, t) => sum + (t.speed || 0), 0),
    };
  }, [tasks]);

  const visible = useMemo(() => {
    const matching = tasks.filter((t) => {
      if (filter === "active") {
        return t.status === "downloading" || t.status === "queued";
      }
      if (filter === "completed") return t.status === "completed";
      if (filter === "failed") return t.status === "failed";
      return true;
    });
    // Failures first: burying them under a long list is how a user misses that
    // a download silently gave up.
    return [...matching].sort((a, b) => {
      if (a.status === "failed" && b.status !== "failed") return -1;
      if (b.status === "failed" && a.status !== "failed") return 1;
      return b.createdAt.localeCompare(a.createdAt);
    });
  }, [tasks, filter]);

  const onNewTask = useCallback(() => {
    // Wired to the new-task dialog in the next step.
  }, []);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "n" && (event.ctrlKey || event.metaKey)) {
        event.preventDefault();
        onNewTask();
      }
      if (event.key === "Escape" && showSettings) {
        setShowSettings(false);
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [onNewTask, showSettings]);

  return (
    <div className="shell">
      <header className="topbar">
        <div className="topbar__brand">
          <span className="topbar__logo">
            <IconDownload size={16} />
          </span>
          VDownload
        </div>

        <div className="topbar__spacer" />

        <span className="badge badge--muted num">
          {version ? `v${version}` : "dev"}
        </span>
      </header>

      <nav className="sidebar" aria-label="任务筛选">
        {NAV.map((item) => (
          <button
            key={item.key}
            type="button"
            className={`nav-item${
              !showSettings && filter === item.key ? " nav-item--active" : ""
            }`}
            aria-current={!showSettings && filter === item.key ? "page" : undefined}
            onClick={() => {
              setShowSettings(false);
              setFilter(item.key as Filter);
            }}
          >
            <span className="nav-item__icon">{item.icon}</span>
            <span className="nav-item__label">{item.label}</span>
            <span className="nav-item__count">
              {counts[item.key as keyof typeof counts] ?? 0}
            </span>
          </button>
        ))}

        <div className="sidebar__spacer" />

        <button
          type="button"
          className={`nav-item${showSettings ? " nav-item--active" : ""}`}
          aria-current={showSettings ? "page" : undefined}
          onClick={() => setShowSettings(true)}
        >
          <span className="nav-item__icon">
            <IconSettings size={18} />
          </span>
          <span className="nav-item__label">设置</span>
        </button>

        <div className="sidebar__stats">
          <div className="sidebar__stat">
            <span className="sidebar__stat-label">活动任务</span>
            <span className="sidebar__stat-value num">{stats.activeCount}</span>
          </div>
          <div className="sidebar__stat">
            <span className="sidebar__stat-label">总速度</span>
            <span className="sidebar__stat-value num">
              {formatSpeed(stats.totalSpeed)}
            </span>
          </div>
        </div>
      </nav>

      <main className="main">
        {showSettings ? (
          <div className="empty">
            <span className="empty__icon">
              <IconSettings size={48} />
            </span>
            <p className="empty__title">设置</p>
            <p className="empty__hint">设置页将在下一步实现。</p>
          </div>
        ) : (
          <>
            <div className="toolbar">
              <button
                type="button"
                className="btn btn--primary"
                onClick={onNewTask}
              >
                <IconPlus size={16} />
                新建下载
              </button>
              <span className="toolbar__spacer" />
            </div>

            {visible.length === 0 ? (
              <div className="empty">
                <span className="empty__icon">
                  <IconDownload size={48} />
                </span>
                <p className="empty__title">把下载链接粘贴到这里</p>
                <p className="empty__hint">
                  支持 HTTP / HTTPS 直链。多连接并行下载，中断后可继续；
                  也可以把链接拖进窗口，或在设置里打开剪贴板监控自动捕获。
                </p>
                <div className="empty__actions">
                  <button
                    type="button"
                    className="btn btn--primary"
                    onClick={onNewTask}
                  >
                    <IconPlus size={16} />
                    新建下载
                  </button>
                </div>
              </div>
            ) : (
              <div className="list">
                {/* Task rows arrive with the manager in the next step. */}
              </div>
            )}
          </>
        )}
      </main>
    </div>
  );
}

export default App;
