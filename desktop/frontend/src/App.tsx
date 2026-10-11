import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";
import { EVENTS, api, on } from "./api";
import { NewTaskDialog } from "./components/NewTaskDialog";
import { SettingsPage } from "./components/SettingsPage";
import { Sidebar } from "./components/Sidebar";
import { TaskList } from "./components/TaskList";
import { formatSpeed } from "./format";
import type { Config, Filter, SortDir, SortKey, TaskView } from "./types";
import { extFilter, fileExt, filterExt, taskLabel } from "./types";
import { IconDownload, IconPause, IconPlan, IconPlay, IconPlus } from "./components/Icons";

function App(): JSX.Element {
  const [tasks, setTasks] = useState<TaskView[]>([]);
  const [config, setConfig] = useState<Config | null>(null);

  const [filter, setFilter] = useState<Filter>("all");
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [sortKey, setSortKey] = useState<SortKey>("added");
  const [sortDir, setSortDir] = useState<SortDir>("desc");
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const [dialogOpen, setDialogOpen] = useState(false);
  const [dialogURL, setDialogURL] = useState("");
  const [pendingURL, setPendingURL] = useState("");
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);
  // The sidebar width lives here, not inside Sidebar, because the grid column
  // that sizes it is defined on .shell. Setting a width on the sidebar element
  // instead would leave the grid track at its old size and the two columns
  // would overlap.
  const [sidebarWidth, setSidebarWidth] = useState(224);

  /** The extension the sidebar is filtering by, or "" for a status view. */
  const extActive = filterExt(filter);

  const reload = useCallback(async () => {
    try {
      setTasks(await api.listTasks());
    } catch (e) {
      console.error(e);
    }
  }, []);

  useEffect(() => {
    void api
      .getConfig()
      .then((c) => setConfig(c))
      .catch(() => setConfig(null));
    void reload();
  }, [reload]);

  // A single set of subscriptions drives every list update. Progress events
  // arrive several times a second per task, so they patch one entry in place
  // rather than reloading the whole list.
  useEffect(() => {
    const offs = [
      on<TaskView>(EVENTS.progress, (v) =>
        setTasks((prev) => prev.map((t) => (t.id === v.id ? v : t))),
      ),
      on<TaskView>(EVENTS.done, (v) =>
        setTasks((prev) => prev.map((t) => (t.id === v.id ? v : t))),
      ),
      on<TaskView>(EVENTS.failed, (v) =>
        setTasks((prev) => prev.map((t) => (t.id === v.id ? v : t))),
      ),
      on<TaskView>(EVENTS.added, (v) =>
        setTasks((prev) => (prev.some((t) => t.id === v.id) ? prev : [v, ...prev])),
      ),
      on<string>(EVENTS.removed, (id) =>
        setTasks((prev) => prev.filter((t) => t.id !== id)),
      ),
      // A captured link either opens the dialog for confirmation or is acted
      // on by the Go side, depending on the settings.
      on<string>(EVENTS.clipboardURL, (url) => {
        setPendingURL(url);
        setDialogURL(url);
        setDialogOpen(true);
      }),
    ];
    return () => offs.forEach((off) => off());
  }, []);

  const visible = useMemo(() => {
    let list = tasks;
    if (filter === "active") {
      list = tasks.filter(
        (t) => t.status === "downloading" || t.status === "queued",
      );
    } else if (filter === "completed") {
      list = tasks.filter((t) => t.status === "completed");
    } else if (filter === "failed") {
      list = tasks.filter((t) => t.status === "failed");
    } else if (extActive) {
      list = tasks.filter((t) => fileExt(taskLabel(t)) === extActive);
    }
    return list;
  }, [tasks, filter, extActive]);

  // Every extension present in the list, with how many tasks carry it. Built
  // from the tasks rather than from a fixed table, so a format nobody thought
  // to enumerate still gets a button.
  const extCounts = useMemo(() => {
    const counts = new Map<string, number>();
    for (const t of tasks) {
      const e = fileExt(taskLabel(t));
      if (e) counts.set(e, (counts.get(e) ?? 0) + 1);
    }
    return [...counts.entries()]
      .map(([ext, count]) => ({ ext, count }))
      .sort((a, b) => b.count - a.count || a.ext.localeCompare(b.ext));
  }, [tasks]);

  const stats = useMemo(() => {
    const active = tasks.filter((t) => t.status === "downloading");
    return {
      activeCount: active.length,
      totalSpeed: active.reduce((s, t) => s + (t.speed || 0), 0),
      total: tasks.length,
    };
  }, [tasks]);

  /** Cycles the sort: a new column starts descending, the same column flips. */
  const onSort = useCallback(
    (key: SortKey) => {
      if (key === sortKey) {
        setSortDir((d) => (d === "asc" ? "desc" : "asc"));
        return;
      }
      setSortKey(key);
      setSortDir(key === "name" ? "asc" : "desc");
    },
    [sortKey],
  );

  const onSelect = useCallback((id: string, additive: boolean) => {
    setSelected((prev) => {
      const next = additive ? new Set(prev) : new Set<string>();
      if (additive && prev.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const onNewTask = useCallback(() => {
    setDialogURL(pendingURL);
    setDialogOpen(true);
  }, [pendingURL]);

  // A short-lived line under the toolbar. The bulk actions have nothing else to
  // report through: "start all" on an empty list looks identical to a click
  // that did nothing.
  const [notice, setNotice] = useState("");
  const noticeTimer = useRef<number | null>(null);
  const flash = useCallback((message: string) => {
    setNotice(message);
    if (noticeTimer.current !== null) window.clearTimeout(noticeTimer.current);
    noticeTimer.current = window.setTimeout(() => setNotice(""), 5000);
  }, []);
  useEffect(
    () => () => {
      if (noticeTimer.current !== null) window.clearTimeout(noticeTimer.current);
    },
    [],
  );

  const onStartAll = useCallback(async () => {
    try {
      const n = await api.startAllTasks();
      flash(n > 0 ? `已开始 ${n} 个任务` : "没有等待中的任务");
    } catch (e) {
      flash(`开始失败：${e instanceof Error ? e.message : String(e)}`);
    }
  }, [flash]);

  const onStopAll = useCallback(async () => {
    try {
      const n = await api.stopAllTasks();
      flash(n > 0 ? `已暂停 ${n} 个任务` : "没有正在下载的任务");
    } catch (e) {
      flash(`暂停失败：${e instanceof Error ? e.message : String(e)}`);
    }
  }, [flash]);

  const onStartPlan = useCallback(async () => {
    try {
      const n = await api.startPlan();
      flash(`计划已启动 · ${n} 个任务，全部结束后执行设置里的动作`);
    } catch (e) {
      flash(`计划未能启动：${e instanceof Error ? e.message : String(e)}`);
    }
  }, [flash]);

  // Keyboard shortcuts from the design spec.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const tag = (e.target as HTMLElement | null)?.tagName;
      const typing = tag === "INPUT" || tag === "TEXTAREA";
      if (e.key === "n" && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        setDialogURL("");
        setDialogOpen(true);
      }
      if (e.key === "Escape" && settingsOpen) setSettingsOpen(false);
      if (e.key === "Escape" && dialogOpen) setDialogOpen(false);
      // Space toggles pause on a single selected task.
      if (e.key === " " && !typing && selected.size === 1) {
        e.preventDefault();
        const id = [...selected][0];
        const t = tasks.find((x) => x.id === id);
        if (!t) return;
        if (t.status === "downloading") void api.pauseTask(id).then(reload);
        else if (t.status === "paused") void api.resumeTask(id).then(reload);
      }
      // Delete removes the selected tasks.
      if (e.key === "Delete" && !typing && selected.size > 0) {
        e.preventDefault();
        void Promise.all(
          [...selected].map((id) => api.removeTask(id, false)),
        ).then(() => {
          setSelected(new Set());
          void reload();
        });
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [settingsOpen, dialogOpen, selected, tasks, reload]);

  const hasTasks = tasks.length > 0;

  return (
    <div
      className={`shell${sidebarCollapsed ? " shell--collapsed" : ""}`}
      style={{ "--sidebar-w": `${sidebarWidth}px` } as React.CSSProperties}
    >
      <Sidebar
        tasks={tasks}
        filter={filter}
        extCounts={extCounts}
        activeCount={stats.activeCount}
        totalSpeed={stats.totalSpeed}
        settingsOpen={settingsOpen}
        collapsed={sidebarCollapsed}
        width={sidebarWidth}
        onWidthChange={setSidebarWidth}
        onToggleCollapsed={() => setSidebarCollapsed((v) => !v)}
        onFilter={(f) => {
          setSettingsOpen(false);
          setFilter(f);
        }}
        onOpenSettings={() => setSettingsOpen(true)}
      />

      <main className="main">
        {settingsOpen ? (
          <SettingsPage
            config={config}
            onSaved={(c) => {
              setConfig(c);
              // Re-read from the Go side: normalisation may have adjusted
              // values the user typed.
              void api.getConfig().then(setConfig).catch(() => undefined);
            }}
          />
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

              <span className="toolbar__divider" />

              {/* Bulk actions. Each reports what it did through the notice line,
                  because a click that finds nothing to do is otherwise
                  indistinguishable from one that failed. */}
              <button
                type="button"
                className="btn"
                onClick={() => void onStartAll()}
                title="开始所有等待中的任务"
              >
                <IconPlay size={16} />
                全部开始
              </button>
              <button
                type="button"
                className="btn"
                onClick={() => void onStopAll()}
                title="暂停所有正在下载的任务，保留断点"
              >
                <IconPause size={16} />
                全部停止
              </button>
              <button
                type="button"
                className="btn btn--plan"
                onClick={() => void onStartPlan()}
                title="下载全部任务，全部结束后执行设置里的动作"
              >
                <IconPlan size={16} />
                计划下载
              </button>

              <span className="toolbar__spacer" />
              {notice ? (
                <span className="toolbar__notice">{notice}</span>
              ) : (
                <span className="toolbar__summary num">
                  {stats.total > 0
                    ? `${stats.total} 个任务 · ${formatSpeed(stats.totalSpeed)}`
                    : "暂无任务"}
                </span>
              )}
            </div>

            {!hasTasks ? (
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
            ) : visible.length === 0 ? (
              <div className="empty">
                <p className="empty__title">这个分类下没有任务</p>
                <p className="empty__hint">
                  换个筛选条件看看，或点左上角「全部」。
                </p>
              </div>
            ) : (
              <TaskList
                tasks={visible}
                sortKey={sortKey}
                sortDir={sortDir}
                onSort={onSort}
                selected={selected}
                onSelect={onSelect}
                groupByType={filter === "all"}
                onChanged={() => void reload()}
              />
            )}
          </>
        )}
      </main>

      <NewTaskDialog
        open={dialogOpen}
        initialURL={dialogURL}
        config={config}
        onClose={() => {
          setDialogOpen(false);
          setPendingURL("");
          // A captured link that the user declined must not come back on the
          // next poll.
        }}
        onCreated={() => void reload()}
      />
    </div>
  );
}

export default App;
