import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { JSX } from "react";
import { EVENTS, api, on } from "./api";
import { ConfirmDialog } from "./components/ConfirmDialog";
import { NewTaskDialog } from "./components/NewTaskDialog";
import { SettingsPage } from "./components/SettingsPage";
import { Sidebar } from "./components/Sidebar";
import { TaskList } from "./components/TaskList";
import { UpdateNotesDialog } from "./components/UpdateNotesDialog";
import { filterLabel } from "./filterLabel";
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

function App(): JSX.Element {
  const [tasks, setTasks] = useState<TaskView[]>([]);
  const [config, setConfig] = useState<Config | null>(null);
  // Latest config for the bridge handlers, which subscribe exactly once:
  // re-subscribing on every config change would churn the listeners, and the
  // clipboard handler has to read the setting in force when the link arrives.
  const configRef = useRef<Config | null>(null);
  configRef.current = config;

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
  const [dialogSize, setDialogSize] = useState(0);
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
  /** The version whose "what's new" dialog is open, from the banner. */
  const [updateNotesFor, setUpdateNotesFor] = useState<string | null>(null);
  /** One-sentence report on the last update attempt; empty when there is none. */
  const [updateNoticeMsg, setUpdateNoticeMsg] = useState("");
  /** The bulk-delete confirmation; the count in it comes from the live list. */
  const [confirmDeleteOpen, setConfirmDeleteOpen] = useState(false);
  /** Generation counter for list reloads; see reload below. */
  const reloadGen = useRef(0);
  const [deleting, setDeleting] = useState(false);
  // Guards runSelected against double submission (double-click, key repeat).
  const bulkBusyRef = useRef(false);
  const [pendingURL, setPendingURL] = useState("");
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);
  /** Where the sidebar width is remembered between sessions. */
  const SIDEBAR_W_KEY = "vkmdownload.sidebar-width";
  function loadSidebarWidth(): number {
    try {
      const raw = localStorage.getItem(SIDEBAR_W_KEY);
      if (raw) {
        const n = Number(raw);
        if (Number.isFinite(n)) return Math.max(180, Math.min(420, n));
      }
    } catch {
      // Storage can be unavailable; the width simply will not persist.
    }
    return 224;
  }
  // The sidebar width lives here, not inside Sidebar, because the grid column
  // that sizes it is defined on .shell. Setting a width on the sidebar element
  // instead would leave the grid track at its old size and the two columns
  // would overlap.
  const [sidebarWidth, setSidebarWidth] = useState(loadSidebarWidth);
  useEffect(() => {
    // Debounced: the sidebar drag updates this per frame; persisting per frame
    // was pure JSON.stringify + disk churn.
    const id = window.setTimeout(() => {
      try {
        localStorage.setItem(SIDEBAR_W_KEY, String(sidebarWidth));
      } catch {
        // Storage can be unavailable; the width simply will not persist.
      }
    }, 300);
    return () => window.clearTimeout(id);
  }, [sidebarWidth, SIDEBAR_W_KEY]);

  /** The extension the sidebar is filtering by, or "" for a status view. */
  const extActive = filterExt(filter);

  const reload = useCallback(async () => {
    // Overlapping reloads race: a slow earlier call resolving after a later
    // one (or after a task:done event) would paint stale rows back over the
    // truth — e.g. a finished row flipped to downloading forever, since no
    // further event for it will ever arrive. Only the latest call applies.
    const gen = ++reloadGen.current;
    try {
      const list = await api.listTasks();
      if (gen !== reloadGen.current) return;
      setTasks(list);
    } catch (e) {
      console.error(e);
    }
  }, []);

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

  // The newest release announced so far. Both the startup check and the
  // backend event report it; without dedupe an offline-then-online startup
  // paints the banner twice for one release, and without the event the
  // banner never appears when the startup check ran before the network did.
  const seenUpdateVersion = useRef<string | null>(null);
  const noteUpdate = useCallback((info: UpdateInfo | null | undefined) => {
    if (!info || !info.available || !info.version) return;
    if (seenUpdateVersion.current === info.version) return;
    seenUpdateVersion.current = info.version;
    setUpdate(info);
  }, []);

  /** One link arrival, however it reached the app. */
  interface Arrival {
    url: string;
    filename: string;
    referer: string;
    cookie: string;
    userAgent: string;
    /** Browser-known total size in bytes; 0 means unknown. */
    size: number;
    /** True when an intercepted download is asking where to go. */
    ask: boolean;
  }
  // Arrivals that came in while the dialog was already open. Closing the
  // dialog drains the queue instead of dropping them on the floor.
  const arrivalQueue = useRef<Arrival[]>([]);
  // Mirrors dialogOpen for event handlers: two arrivals landing in the same
  // tick would both read the still-false state and overwrite each other.
  const dialogOpenRef = useRef(false);
  const applyArrival = useCallback((a: Arrival) => {
    setPendingURL(a.url);
    setDialogURL(a.url);
    setDialogFilename(a.filename);
    setDialogReferer(a.referer);
    setDialogCookie(a.cookie);
    setDialogUserAgent(a.userAgent);
    setDialogSize(a.size);
    setDialogAsk(a.ask);
    dialogOpenRef.current = true;
    setDialogOpen(true);
    // The user is in another app; pull the question in front of it.
    void api.bringToFront().catch(() => {});
  }, []);
  // A captured link either opens the dialog for confirmation or waits its
  // turn: overwriting the dialog would silently discard what the user already
  // typed there.
  const handleArrival = useCallback(
    (a: Arrival) => {
      if (dialogOpenRef.current) {
        arrivalQueue.current.push(a);
        flash("收到新链接，先处理当前对话框");
        return;
      }
      applyArrival(a);
    },
    [applyArrival, flash],
  );

  useEffect(() => {
    void api
      .getConfig()
      .then((c) => setConfig(c))
      .catch(() => setConfig(null));
    // Asked once: the version cannot change while the app is running.
    // This direct check complements the update:available event below: an
    // event fired before the subscription exists would be missed silently
    // (e.g. offline at startup), and noteUpdate dedupes by version so the
    // two triggers never paint the banner twice for one release.
    void api
      .checkUpdate()
      .then((info) => noteUpdate(info))
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
    // What became of the last update attempt (rolled back, never confirmed,
    // never started), pulled once: the backend hands it out exactly once per
    // attempt, and a pull here is the only channel a page that mounts after
    // startup can still receive.
    void api
      .consumeUpdateNotice()
      .then((msg) => {
        if (msg) setUpdateNoticeMsg(msg);
      })
      .catch(() => {
        // No notice or a bridge hiccup is not worth surfacing on its own.
      });
    void reload();
  }, [reload, noteUpdate]);

  // A single set of subscriptions drives every list update. Progress events
  // arrive several times a second per task, so they patch one entry in place
  // rather than reloading the whole list.
  useEffect(() => {
    const offs = [
      on<TaskView>(EVENTS.progress, (v) =>
        setTasks((prev) =>
          prev.map((t) => {
            if (t.id !== v.id) return t;
            // A finished row never goes back to transferring: progress events
            // travel through a queue while done jumps it directly, so a slow
            // stale update arriving late must not resurrect it. (Failed rows
            // are not frozen — resume legitimately returns them to downloading
            // — but a late event must not undo a failure the user can see: the
            // transition is only accepted when its byte count is not behind.
            // The backend guards this too; this just stops the row flickering.)
            if (t.status === "completed" && v.status !== "completed") return t;
            if (
              t.status === "failed" &&
              v.status === "downloading" &&
              (v.downloaded || 0) < (t.downloaded || 0)
            )
              return t;
            return v;
          }),
        ),
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
      // A captured clipboard link. The backend has already acted on the
      // clipboard settings before it emitted this: with "先弹窗确认" it created
      // nothing and expects the dialog, but otherwise it has already added
      // (and possibly started) the task, so opening the dialog here would ask
      // about a download that already exists — and its "取消" would not cancel
      // anything. Read the current config through the ref so the handler keeps
      // seeing the setting in force.
      on<string>(EVENTS.clipboardURL, (url) => {
        const cfg = configRef.current;
        // While the config is still loading the setting is unknown; falling
        // back to the dialog is the safe side (a redundant prompt beats a link
        // silently going nowhere).
        if (cfg && !cfg.clipboardConfirm) {
          flash(
            cfg.clipboardAutoStart
              ? "已按设置开始下载剪贴板里的链接"
              : "已按设置把剪贴板里的链接加入列表",
          );
          return;
        }
        handleArrival({
          url,
          filename: "",
          referer: "",
          cookie: "",
          userAgent: "",
          size: 0,
          ask: false,
        });
      }),
      // Ask-location arrivals (extension or clipboard): nothing is created
      // yet. The dialog collects the placement; closing it abandons the
      // download, which is what the user chose.
      on<{ url: string; filename: string; referer: string; cookie: string; userAgent: string; size: string }>(
        EVENTS.locationAsk,
        (req) => {
          handleArrival({
            url: req.url,
            filename: req.filename ?? "",
            referer: req.referer ?? "",
            cookie: req.cookie ?? "",
            userAgent: req.userAgent ?? "",
            size: Number(req.size) || 0,
            ask: true,
          });
        },
      ),
      // Files dragged onto the window. The backend emits one event per path;
      // a dropped path is not itself a download, so only an http(s) link
      // found inside it opens the dialog — anything else is ignored aloud
      // rather than failing silently.
      on<string>(EVENTS.filesDropped, (text) => {
        const raw = typeof text === "string" ? text : String(text ?? "");
        const m = /https?:\/\/\S+/i.exec(raw);
        if (!m) {
          flash("拖入的内容里没有可识别的下载链接，已忽略");
          return;
        }
        handleArrival({
          url: m[0],
          filename: "",
          referer: "",
          cookie: "",
          userAgent: "",
          size: 0,
          ask: false,
        });
      }),
      // The backend finished restoring tasks.json after this page mounted:
      // the initial listTasks can have seen an empty table while the
      // restore was still running, and nothing else would ever correct it.
      on<null>(EVENTS.tasksLoaded, () => {
        void reload();
      }),
      // A release announced after startup (e.g. the startup check ran while
      // offline). Deduped by version against the startup check.
      on<UpdateInfo>(EVENTS.updateAvailable, (info) => {
        noteUpdate(info);
      }),
    ];
    return () => offs.forEach((off) => off());
  }, [handleArrival, noteUpdate, reload]);

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
    } else if (filter === "canceled") {
      list = tasks.filter((t) => t.status === "canceled");
    } else if (extActive) {
      list = tasks.filter((t) => fileExt(taskLabel(t)) === extActive);
    }
    return list;
  }, [tasks, filter, extActive]);

  // One pass over the tasks produces the extension list, the totals and the
  // sidebar counts together. Progress events rewrite the array several times
  // a second, and a separate scan per derived value used to mean a full walk
  // per value on every tick.
  const summary = useMemo(() => {
    const exts = new Map<string, number>();
    let activeCount = 0;
    let totalSpeed = 0;
    let active = 0;
    let completed = 0;
    let failed = 0;
    let canceled = 0;
    for (const t of tasks) {
      // Every extension present in the list, with how many tasks carry it.
      // Built from the tasks rather than from a fixed table, so a format
      // nobody thought to enumerate still gets a button.
      const e = fileExt(taskLabel(t));
      if (e) exts.set(e, (exts.get(e) ?? 0) + 1);
      if (t.status === "downloading") {
        activeCount += 1;
        active += 1;
        totalSpeed += t.speed || 0;
      } else if (t.status === "queued") {
        active += 1;
      } else if (t.status === "completed") {
        completed += 1;
      } else if (t.status === "failed") {
        failed += 1;
      } else if (t.status === "canceled") {
        canceled += 1;
      }
    }
    return {
      extCounts: [...exts.entries()]
        .map(([ext, count]) => ({ ext, count }))
        .sort((a, b) => b.count - a.count || a.ext.localeCompare(b.ext)),
      stats: {
        activeCount,
        totalSpeed,
        total: tasks.length,
        failed,
      },
      sidebarCounts: {
        all: tasks.length,
        active,
        completed,
        failed,
        canceled,
      },
    };
  }, [tasks]);
  const { extCounts, stats } = summary;

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

  // Stable across renders so memoized rows below don't reconcile on every
  // tick: an inline arrow would be a new reference each time and defeat memo.
  const handleChanged = useCallback(() => void reload(), [reload]);

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
    // Same ref the captured-link path sets: a clipboard capture or extension
    // handover arriving while the user fills this manually-opened form must
    // queue behind it, not reset it mid-typing.
    dialogOpenRef.current = true;
    setDialogOpen(true);
  }, [pendingURL]);

  // Drop selected ids that no longer exist (deleted elsewhere, list
  // reloaded): without this the toolbar keeps acting on ghosts.
  useEffect(() => {
    setSelected((prev) => {
      if (prev.size === 0) return prev;
      const alive = new Set(tasks.map((t) => t.id));
      let dropped = false;
      const next = new Set<string>();
      prev.forEach((id) => {
        if (alive.has(id)) next.add(id);
        else dropped = true;
      });
      return dropped ? next : prev;
    });
  }, [tasks]);

  /** Which list the delete confirmation currently covers. */
  const [deleteMode, setDeleteMode] = useState<"visible" | "selected">(
    "visible",
  );
  // Selected-delete and the Delete key share one confirmation: a single key
  // press can remove as many rows as the toolbar button, so it asks first
  // the same way. runSelected still reports through the notice line.
  const requestDeleteSelected = useCallback(() => {
    if (selected.size === 0) return;
    setDeleteMode("selected");
    setConfirmDeleteOpen(true);
  }, [selected.size]);

  // Bulk verbs over the current selection. Per-task calls are settled
  // individually so one bad row cannot abort the rest; the notice reports
  // how many actually went through. Removing also clears the selection,
  // since selected ids would otherwise point at rows that no longer exist.
  const runSelected = useCallback(
    async (action: "resume" | "pause" | "remove") => {
      const ids = [...selected];
      if (ids.length === 0) return;
      // One batch at a time: without this, double-clicking (or keyboard
      // repeat) submits the same ids twice and the counts lie.
      if (bulkBusyRef.current) return;
      bulkBusyRef.current = true;
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
      bulkBusyRef.current = false;
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
            // Reaching this line means the process could not quit for the
            // swap — a normal apply ends by the app exiting. Report it rather
            // than leaving the banner stuck on "正在应用…" forever.
            setUpdating(false);
            flash("应用更新失败：程序没有退出，可稍后在设置里重试");
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

  // Latest values for the global key handler, which subscribes exactly once:
  // re-subscribing per render (tasks changes on every progress tick) churned
  // the listener on every frame that moved a progress bar.
  const keyRef = useRef({
    tasks,
    selected,
    settingsOpen,
    dialogOpen,
    onNewTask,
    requestDeleteSelected,
    reload,
  });
  keyRef.current = {
    tasks,
    selected,
    settingsOpen,
    dialogOpen,
    onNewTask,
    requestDeleteSelected,
    reload,
  };

  // Keyboard shortcuts from the design spec.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const { tasks, selected, settingsOpen, dialogOpen, onNewTask, requestDeleteSelected, reload } =
        keyRef.current;
      const tag = (e.target as HTMLElement | null)?.tagName;
      const typing = tag === "INPUT" || tag === "TEXTAREA";
      if (e.key === "n" && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        // Same entry as the toolbar button: a pending captured link wins in
        // both, and both leave the clearing to the dialog's onClose.
        onNewTask();
      }
      if (e.key === "Escape" && settingsOpen) setSettingsOpen(false);
      if (e.key === "Escape" && dialogOpen) {
        dialogOpenRef.current = false;
        setDialogOpen(false);
      }
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
      // Delete removes the selected tasks, through the same confirmation
      // as the toolbar button.
      if (e.key === "Delete" && !typing && selected.size > 0) {
        e.preventDefault();
        requestDeleteSelected();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const hasTasks = tasks.length > 0;

  return (
    <div
      className={`shell${sidebarCollapsed ? " shell--collapsed" : ""}`}
      style={{ "--sidebar-w": `${sidebarWidth}px` } as React.CSSProperties}
    >
      <Sidebar
        counts={summary.sidebarCounts}
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
        {updateNoticeMsg ? (
          // What became of the last update attempt, told once by the backend
          // and kept on screen until dismissed: a rollback notice that fades
          // after five seconds is a notice nobody reads.
          <div className="updatebar" role="status">
            <span>{updateNoticeMsg}</span>
            <span className="updatebar__actions">
              <button
                type="button"
                className="btn"
                onClick={() => setUpdateNoticeMsg("")}
              >
                知道了
              </button>
            </span>
          </div>
        ) : null}
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
                  onClick={() => setUpdateNotesFor(update.version)}
                >
                  立即更新
                </button>
              ) : null}
              {!updating && update.url ? (
                <button
                  type="button"
                  className="btn"
                  onClick={() => {
                    void api.openReleasePage(update.url).catch((e) =>
                      flash(
                        `打开发布页失败：${e instanceof Error ? e.message : String(e)}`,
                      ),
                    );
                  }}
                >
                  去发布页看看
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
            updateNotice={updateNoticeMsg}
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
                  onClick={() => {
                    if (selected.size > 0) requestDeleteSelected();
                    else {
                      setDeleteMode("visible");
                      setConfirmDeleteOpen(true);
                    }
                  }}
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
                  把含下载链接的文件拖进窗口会尝试识别其中的链接，也可以在设置里
                  打开剪贴板监控，复制链接后自动捕获。
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
                <div className="empty__actions">
                  <button
                    type="button"
                    className="btn"
                    onClick={() => setFilter("all")}
                  >
                    回到全部
                  </button>
                </div>
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
                onChanged={handleChanged}
                onError={flash}
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
        initialSize={dialogSize}
        fromAsk={dialogAsk}
        config={config}
        onClose={() => {
          dialogOpenRef.current = false;
          setDialogOpen(false);
          setPendingURL("");
          setDialogAsk(false);
          // A captured link that the user declined must not come back on the
          // next poll. Links that arrived while the dialog was open were
          // queued instead of overwriting it; the next one opens now.
          const next = arrivalQueue.current.shift();
          if (next) applyArrival(next);
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
        deleteMode === "selected" ? (
          <ConfirmDialog
            title="删除选中记录？"
            message={`将从列表里删除选中的 ${selected.size} 条记录。`}
            detail="只删除列表记录。下好的文件会保留；没下完的半成品和续传记录会一起删掉，同链接重下从头开始。"
            confirmLabel={`删除 ${selected.size} 条`}
            onConfirm={() => {
              setConfirmDeleteOpen(false);
              void runSelected("remove");
            }}
            onCancel={() => setConfirmDeleteOpen(false)}
          />
        ) : (
          <ConfirmDialog
            title="删除这些记录？"
            message={`将从列表里删除${filter === "all" ? "全部" : filterLabel(filter)} ${visible.length} 条记录。`}
            detail="只删除列表记录。下好的文件会保留；没下完的半成品和续传记录会一起删掉，同链接重下从头开始。"
            confirmLabel={`删除 ${visible.length} 条`}
            busy={deleting}
            onConfirm={() => void onDeleteVisible()}
            onCancel={() => setConfirmDeleteOpen(false)}
          />
        )
      ) : null}

      {updateNotesFor ? (
        <UpdateNotesDialog
          version={updateNotesFor}
          current={version}
          onClose={() => setUpdateNotesFor(null)}
          onUpdate={() => {
            setUpdateNotesFor(null);
            void startUpdateNow();
          }}
        />
      ) : null}
    </div>
  );
}

export default App;
