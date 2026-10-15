import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";
import { EVENTS, api, on } from "./api";
import { ConfirmDialog } from "./components/ConfirmDialog";
import { NewTaskDialog } from "./components/NewTaskDialog";
import { SettingsPage } from "./components/SettingsPage";
import { Sidebar } from "./components/Sidebar";
import { TaskList } from "./components/TaskList";
import { formatSpeed } from "./format";
import type { Config, Filter, SortDir, SortKey, TaskView, UpdateInfo } from "./types";
import { extFilter, fileExt, filterExt, taskLabel } from "./types";
import {
  IconDownload,
  IconPause,
  IconPlan,
  IconPlay,
  IconPlus,
  IconTrash,
} from "./components/Icons";

/** Names a filter for the bulk-delete button and its confirmation. */
function filterLabel(filter: Filter): string {
  switch (filter) {
    case "all":
      return "全部";
    case "active":
      return "下载中";
    case "completed":
      return "已完成";
    case "failed":
      return "失败";
    default: {
      const ext = filterExt(filter);
      return ext ? `${ext} 文件` : "当前列表";
    }
  }
}

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
  /** Prefill for the ask-location flow; empty for a manual dialog. */
  const [dialogFilename, setDialogFilename] = useState("");
  const [dialogReferer, setDialogReferer] = useState("");
  /** Browser credentials carried through the ask-location flow; never shown. */
  const [dialogCookie, setDialogCookie] = useState("");
  const [dialogUserAgent, setDialogUserAgent] = useState("");
  const [dialogAsk, setDialogAsk] = useState(false);
  /** From the binding; shown at the foot of the sidebar. */
  const [version, setVersion] = useState("");
  /** A newer release announced by the Go side; null until one arrives. */
  const [update, setUpdate] = useState<UpdateInfo | null>(null);
  /** The "where to update later" hint after dismissing the banner. */
  const [updateHintOpen, setUpdateHintOpen] = useState(false);
  /** One-click update in flight from the banner; progress shows in the bar. */
  const [updating, setUpdating] = useState(false);
  const [updPct, setUpdPct] = useState("");
  /** Set when the banner is dismissed mid-download: the staging keeps going
      in the backend, but the automatic restart is called off. */
  const updateCancelled = useRef(false);
  /** The bulk-delete confirmation; the count in it comes from the live list. */
  const [confirmDeleteOpen, setConfirmDeleteOpen] = useState(false);
  const [deleting, setDeleting] = useState(false);
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
    // Asked once: the version cannot change while the app is running.
    // The update banner has two triggers: the event from the Go side, and
    // this direct check. The event alone is not enough — it can fire before
    // the subscription below exists and would then be missed silently.
    void api
      .checkUpdate()
      .then((info) => {
        if (info && info.available) setUpdate(info);
      })
      .catch(() => {
        // Offline is not an error worth surfacing.
      });
    // Asked once: the version cannot change while the app is running.
    void api
      .version()
      .then((v) => setVersion(v))
      .catch(() => {
        // A missing version is not worth surfacing; the footer just stays empty.
      });
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
        setDialogFilename("");
        setDialogReferer("");
        setDialogCookie("");
        setDialogUserAgent("");
        setDialogAsk(false);
        setDialogOpen(true);
        // The user is in another app; pull the question in front of it.
        void api.bringToFront().catch(() => {});
      }),
      // Ask-location arrivals (extension or clipboard): nothing is created
      // yet. The dialog collects the placement; closing it abandons the
      // download, which is what the user chose.
      on<{ url: string; filename: string; referer: string; cookie: string; userAgent: string }>(
        EVENTS.locationAsk,
        (req) => {
          setPendingURL(req.url);
          setDialogURL(req.url);
          setDialogFilename(req.filename ?? "");
          setDialogReferer(req.referer ?? "");
          setDialogCookie(req.cookie ?? "");
          setDialogUserAgent(req.userAgent ?? "");
          setDialogAsk(true);
          setDialogOpen(true);
          // The user is in the browser; pull the question in front of it.
          void api.bringToFront().catch(() => {});
        },
      ),
      // A newer release, announced once per run by the Go side.
      on<UpdateInfo>(EVENTS.updateAvailable, (info) => {
        if (info && info.available) setUpdate(info);
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
      failed: tasks.filter((t) => t.status === "failed").length,
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

  // Bulk verbs over the current selection. Per-task calls are settled
  // individually so one bad row cannot abort the rest; the notice reports
  // how many actually went through. Removing also clears the selection,
  // since selected ids would otherwise point at rows that no longer exist.
  const runSelected = useCallback(
    async (action: "resume" | "pause" | "remove") => {
      const ids = [...selected];
      if (ids.length === 0) return;
      const verbs = { resume: "继续", pause: "暂停", remove: "删除" } as const;
      let ok = 0;
      await Promise.all(
        ids.map(async (id) => {
          try {
            if (action === "resume") await api.resumeTask(id);
            else if (action === "pause") await api.pauseTask(id);
            else await api.removeTask(id, false);
            ok++;
          } catch {
            // Count what worked; the row keeps its state for a retry.
          }
        }),
      );
      if (action === "remove") setSelected(new Set());
      void reload();
      flash(ok > 0 ? `已${verbs[action]} ${ok}/${ids.length} 个` : `${verbs[action]}失败`);
    },
    [selected, flash, reload],
  );

  const onStartAll = useCallback(async () => {
    if (selected.size > 0) {
      await runSelected("resume");
      return;
    }
    try {
      const n = await api.startAllTasks();
      flash(n > 0 ? `已开始 ${n} 个任务` : "没有等待中的任务");
    } catch (e) {
      flash(`开始失败：${e instanceof Error ? e.message : String(e)}`);
    }
  }, [flash, selected, runSelected]);

  const onStopAll = useCallback(async () => {
    if (selected.size > 0) {
      await runSelected("pause");
      return;
    }
    try {
      const n = await api.stopAllTasks();
      flash(n > 0 ? `已暂停 ${n} 个任务` : "没有正在下载的任务");
    } catch (e) {
      flash(`暂停失败：${e instanceof Error ? e.message : String(e)}`);
    }
  }, [flash, selected, runSelected]);

  const onStartPlan = useCallback(async () => {
    try {
      const n =
        selected.size > 0
          ? await api.startPlanSelected([...selected])
          : await api.startPlan();
      flash(
        selected.size > 0
          ? `计划已启动 · 选中 ${n} 个任务，结束后执行设置里的动作`
          : `计划已启动 · ${n} 个任务，全部结束后执行设置里的动作`,
      );
    } catch (e) {
      flash(`计划未能启动：${e instanceof Error ? e.message : String(e)}`);
    }
  }, [flash, selected]);

  const onDeleteVisible = useCallback(async () => {
    setDeleting(true);
    try {
      const n = await api.removeTasks(visible.map((t) => t.id));
      setConfirmDeleteOpen(false);
      // Removed rows must leave the selection too, or the toolbar keeps
      // acting on ids that no longer exist.
      setSelected(new Set());
      flash(
        n > 0
          ? `已删除 ${n} 条记录 · 没下完的半成品已清理，下好的文件没动`
          : "没有可删除的记录",
      );
    } catch (e) {
      flash(`删除失败：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setDeleting(false);
    }
  }, [visible, flash]);

  // One-click update from the banner: download, then restart straight into
  // the new version. The backend pauses every task before swapping files, so
  // updating mid-download loses nothing; after the restart everything loads
  // as paused, ready to resume.
  const startUpdateNow = useCallback(async () => {
    updateCancelled.current = false;
    setUpdating(true);
    setUpdPct("");
    try {
      const s = await api.startUpdate();
      if (s.state === "failed") {
        setUpdating(false);
        flash(`更新失败：${s.error || "未知错误"}`);
        return;
      }
      for (;;) {
        await new Promise((r) => setTimeout(r, 800));
        if (updateCancelled.current) {
          setUpdating(false);
          return;
        }
        let p;
        try {
          p = await api.updateProgress();
        } catch {
          continue;
        }
        if (p.state === "ready") {
          setUpdPct("正在应用…");
          try {
            await api.applyUpdate();
          } catch (e) {
            setUpdating(false);
            flash(`更新失败：${e instanceof Error ? e.message : String(e)}`);
          }
          return;
        }
        if (p.state === "failed") {
          setUpdating(false);
          flash(`更新失败：${p.error || "未知错误"}`);
          return;
        }
        if (p.total > 0) {
          setUpdPct(`${Math.floor((p.downloaded / p.total) * 100)}%`);
        }
      }
    } catch (e) {
      setUpdating(false);
      flash(`更新失败：${e instanceof Error ? e.message : String(e)}`);
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
        if (t.status === "downloading" || t.status === "queued")
          void api.pauseTask(id).then(reload);
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
        {update && (
          <div className="updatebar" role="status">
            <span>
              {updating
                ? `正在更新到 ${update.version}${updPct ? ` ${updPct}` : "…"}`
                : `发现新版本 ${update.version}${version ? `（当前 ${version}）` : ""}，更新时会先暂停全部任务，重启后可继续`}
            </span>
            <span className="updatebar__actions">
              {!updating ? (
                <button
                  type="button"
                  className="btn btn--primary"
                  onClick={() => void startUpdateNow()}
                >
                  立即更新
                </button>
              ) : null}
              <button
                type="button"
                className="btn"
                onClick={() => {
                  updateCancelled.current = true;
                  setUpdating(false);
                  setUpdate(null);
                  setUpdateHintOpen(true);
                }}
              >
                不再提示
              </button>
            </span>
          </div>
        )}
        {settingsOpen ? (
          <SettingsPage
            config={config}
            version={version}
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
                  indistinguishable from one that failed.
                  When rows are checked these three switch from "everything"
                  to "the selection", so starting one never pulls the rest
                  along; the plan button below always means everything. */}
              <button
                type="button"
                className="btn"
                onClick={() => void onStartAll()}
                title={
                  selected.size > 0
                    ? `开始选中的 ${selected.size} 个任务`
                    : "开始所有等待中的任务"
                }
              >
                <IconPlay size={16} />
                {selected.size > 0 ? `开始选中(${selected.size})` : "全部开始"}
              </button>
              <button
                type="button"
                className="btn"
                onClick={() => void onStopAll()}
                title={
                  selected.size > 0
                    ? `暂停选中的 ${selected.size} 个任务，保留断点`
                    : "暂停所有正在下载的任务，保留断点"
                }
              >
                <IconPause size={16} />
                {selected.size > 0 ? `停止选中(${selected.size})` : "全部停止"}
              </button>
              <button
                type="button"
                className="btn btn--plan"
                onClick={() => void onStartPlan()}
                title={
                  selected.size > 0
                    ? `只下载选中的 ${selected.size} 个任务，结束后执行设置里的动作`
                    : "下载全部任务，全部结束后执行设置里的动作"
                }
              >
                <IconPlan size={16} />
                {selected.size > 0 ? `计划选中(${selected.size})` : "计划下载"}
              </button>



              {/* Deletes what the list is currently showing, whatever that is.
                  The label names the filter and the count, because "delete
                  all" over an unnoticed filter is how someone loses rows they
                  meant to keep. With checked rows it switches to the
                  selection instead — same rule as 开始/停止 above: finished
                  files stay, unfinished partials go with their records. */}
              {visible.length > 0 || selected.size > 0 ? (
                <button
                  type="button"
                  className="btn btn--danger"
                  onClick={() =>
                    selected.size > 0
                      ? void runSelected("remove")
                      : setConfirmDeleteOpen(true)
                  }
                  title={
                    selected.size > 0
                      ? `删除选中的 ${selected.size} 条记录，下好的文件不动，没下完的半成品会一起清理`
                      : "删除列表里的记录，没下完的半成品会一起清理，下好的文件不动"
                  }
                >
                  <IconTrash size={16} />
                  {selected.size > 0
                    ? `删除选中(${selected.size})`
                    : `删除${filter === "all" ? "全部" : filterLabel(filter)}`}
                  <span className="btn__count">
                    {selected.size > 0 ? selected.size : visible.length}
                  </span>
                </button>
              ) : null}

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
        initialFilename={dialogFilename}
        initialReferer={dialogReferer}
        initialCookie={dialogCookie}
        initialUserAgent={dialogUserAgent}
        fromAsk={dialogAsk}
        config={config}
        onClose={() => {
          setDialogOpen(false);
          setPendingURL("");
          setDialogAsk(false);
          // A captured link that the user declined must not come back on the
          // next poll.
        }}
        onCreated={() => void reload()}
        onLocateTask={(id) => {
          setFilter("all");
          setSelected(new Set([id]));
          void reload();
          flash("该链接已在列表中，已为你选中");
        }}
      />

      {updateHintOpen ? (
        <ConfirmDialog
          title="更新在设置里"
          message="以后想更新时，随时可以手动检查并安装。"
          detail="设置 → 拉到最底部 → 软件更新 → 检查更新。"
          confirmLabel="去设置页"
          onConfirm={() => {
            setUpdateHintOpen(false);
            setSettingsOpen(true);
          }}
          onCancel={() => setUpdateHintOpen(false)}
        />
      ) : null}

      {confirmDeleteOpen ? (
        <ConfirmDialog
          title="删除这些记录？"
          message={`将从列表里删除${filter === "all" ? "全部" : filterLabel(filter)} ${visible.length} 条记录。`}
          detail="只删除列表记录。下好的文件会保留；没下完的半成品和续传记录会一起删掉，同链接重下从头开始。"
          confirmLabel={`删除 ${visible.length} 条`}
          busy={deleting}
          onConfirm={() => void onDeleteVisible()}
          onCancel={() => setConfirmDeleteOpen(false)}
        />
      ) : null}
    </div>
  );
}

export default App;
