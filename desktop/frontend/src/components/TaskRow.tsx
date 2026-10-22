import {
  memo,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import type { CSSProperties, JSX } from "react";
import { ClipboardSetText } from "../../wailsjs/runtime/runtime";
import { api } from "../api";
import { clampMenuPosition, contextActions } from "../contextMenu";
import {
  formatBytes,
  formatEta,
  formatProgress,
  formatSize,
  formatSpeed,
  formatWhen,
  ellipsizeMiddle,
} from "../format";
import { STATUS_LABEL, STATUS_TONE, displayURL, urlTail } from "../types";
import type { TaskStatus, TaskView } from "../types";
import { ConfirmDialog } from "./ConfirmDialog";
import {
  IconAlert,
  IconCheck,
  IconDownload,
  IconFolder,
  IconPause,
  IconPlay,
  IconTrash,
} from "./Icons";

interface Props {
  task: TaskView;
  selected: boolean;
  onSelect: (id: string, additive: boolean) => void;
  /** Adds the row to the selection for a right-click, never toggling it off. */
  onContextSelect: (id: string) => void;
  onChanged: () => void;
  /** Reports a failed row action to the toolbar notice line. */
  onError: (message: string) => void;
  /** Reports a successful row action (today, copying the link). */
  onNotice: (message: string) => void;
}

/** Statuses that carry a progress bar worth showing. */
const PROGRESSED: TaskStatus[] = ["downloading", "paused", "completed", "failed"];

interface MenuItem {
  label: string;
  disabled?: boolean;
  danger?: boolean;
  onSelect: () => void;
}

const MENU_STYLE: CSSProperties = {
  position: "fixed",
  zIndex: 40,
  minWidth: 176,
  padding: 4,
  background: "var(--bg-raised)",
  border: "1px solid var(--border-strong)",
  borderRadius: "var(--radius-md)",
  boxShadow: "var(--shadow-modal)",
  userSelect: "none",
};

const MENU_ITEM_STYLE: CSSProperties = {
  display: "block",
  width: "100%",
  padding: "7px 12px",
  border: 0,
  borderRadius: "var(--radius-sm)",
  background: "transparent",
  fontFamily: "inherit",
  fontSize: 13,
  lineHeight: "18px",
  textAlign: "left",
};

/** One row of the context menu, including its hover/disabled styling. */
function MenuButton({
  item,
  onDone,
}: {
  item: MenuItem;
  onDone: () => void;
}): JSX.Element {
  const [hover, setHover] = useState(false);
  const disabled = item.disabled === true;
  return (
    <button
      type="button"
      role="menuitem"
      disabled={disabled}
      aria-disabled={disabled}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      onClick={() => {
        onDone();
        item.onSelect();
      }}
      style={{
        ...MENU_ITEM_STYLE,
        background: hover && !disabled ? "var(--bg-hover)" : "transparent",
        color: disabled
          ? "var(--text-muted)"
          : item.danger
            ? "var(--danger)"
            : "var(--text-primary)",
        cursor: disabled ? "default" : "pointer",
      }}
    >
      {item.label}
    </button>
  );
}

/**
 * The right-click menu for one row.
 *
 * It measures itself before paint and clamps to the viewport, so a menu opened
 * at the bottom-right corner still fits on screen. Outside click, Escape, a
 * window resize/blur, or a right-click on another row all dismiss it.
 */
function RowContextMenu({
  x,
  y,
  items,
  onClose,
}: {
  x: number;
  y: number;
  items: MenuItem[];
  onClose: () => void;
}): JSX.Element {
  const ref = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState({ left: x, top: y });

  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    setPos(
      clampMenuPosition(
        x,
        y,
        rect.width,
        rect.height,
        window.innerWidth,
        window.innerHeight,
      ),
    );
  }, [x, y]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    const onOutside = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose();
    };
    const onViewport = () => onClose();
    document.addEventListener("keydown", onKey);
    // Capture phase: the menu closes before the row's own click/contextmenu
    // handler runs, so clicking another row does not leave two menus open.
    document.addEventListener("mousedown", onOutside, true);
    document.addEventListener("contextmenu", onOutside, true);
    document.addEventListener("scroll", onViewport, true);
    window.addEventListener("blur", onViewport);
    window.addEventListener("resize", onViewport);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onOutside, true);
      document.removeEventListener("contextmenu", onOutside, true);
      document.removeEventListener("scroll", onViewport, true);
      window.removeEventListener("blur", onViewport);
      window.removeEventListener("resize", onViewport);
    };
  }, [onClose]);

  return (
    <div
      ref={ref}
      role="menu"
      aria-label="任务操作"
      style={{ ...MENU_STYLE, left: pos.left, top: pos.top }}
      onClick={(e) => e.stopPropagation()}
      onContextMenu={(e) => {
        e.preventDefault();
        e.stopPropagation();
      }}
    >
      {items.map((item) => (
        <MenuButton key={item.label} item={item} onDone={onClose} />
      ))}
    </div>
  );
}

/**
 * TaskRow renders one download.
 *
 * Memoized on props: progress events replace the whole tasks array every few
 * hundred milliseconds, and without this every row reconciles on every tick.
 * Unchanged rows keep their object identity in App's patch handler, so the
 * default shallow compare is exactly right — no custom comparator to rot.
 *
 * Three lines: the name and its status, the progress bar, then the numbers.
 * Keeping the height fixed means the list does not reflow as speeds and ETAs
 * change every few hundred milliseconds.
 */
export const TaskRow = memo(function TaskRow({
  task,
  selected,
  onSelect,
  onContextSelect,
  onChanged,
  onError,
  onNotice,
}: Props): JSX.Element {
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [menu, setMenu] = useState<{ x: number; y: number } | null>(null);
  const closeMenu = useCallback(() => setMenu(null), []);
  const completed = task.status === "completed";
  const pct = completed
    ? 100
    : task.size > 0
      ? Math.min(100, (task.downloaded / task.size) * 100)
      : 0;

  const barClass = [
    "progress",
    task.status === "completed" ? "progress--complete" : "",
    task.status === "paused" ? "progress--paused" : "",
    task.status === "failed" ? "progress--failed" : "",
    task.status === "downloading" && task.speed > 0 ? "progress--live" : "",
  ]
    .filter(Boolean)
    .join(" ");

  async function run(fn: () => Promise<void>) {
    try {
      await fn();
    } catch (e) {
      // The row keeps its state for a retry; the notice line says what
      // happened so a dead click does not look like a working one.
      console.error(e);
      onError(`操作失败：${e instanceof Error ? e.message : String(e)}`);
    }
    // Full reload, never an optimistic patch: the row is cheap to re-read
    // and a guessed state is how rows get stuck showing the wrong thing.
    onChanged();
  }

  /** Copies the original URL, credentials and all, exactly as stored. */
  async function copyLink() {
    try {
      const ok = await ClipboardSetText(task.url);
      if (ok) onNotice("已复制下载链接");
      else onError("复制链接失败");
    } catch (e) {
      onError(`复制链接失败：${e instanceof Error ? e.message : String(e)}`);
    }
  }

  const context = contextActions(task.status);
  const menuItems: MenuItem[] = [
    {
      label: context.resumeLabel,
      disabled: !context.canResume,
      onSelect: () => void run(() => api.resumeTask(task.id)),
    },
    {
      label: "暂停",
      disabled: !context.canPause,
      onSelect: () => void run(() => api.pauseTask(task.id)),
    },
    {
      label: "打开所在目录",
      onSelect: () => void run(() => api.revealInExplorer(task.id)),
    },
    {
      label: "复制链接",
      onSelect: () => void copyLink(),
    },
    {
      label: "删除",
      danger: true,
      onSelect: () => setConfirmRemove(true),
    },
  ];

  return (
    <div
      className={`task${selected ? " task--selected" : ""}${
        task.status === "failed" ? " task--failed" : ""
      }`}
      onClick={(e) => onSelect(task.id, e.ctrlKey || e.metaKey)}
      onContextMenu={(e) => {
        // The browser menu would cover ours, and the click must also select
        // the row exactly like the checkbox does (without clearing the rest
        // of a multi-selection).
        e.preventDefault();
        onContextSelect(task.id);
        setMenu({ x: e.clientX, y: e.clientY });
      }}
    >
      <div className="task__cell task__cell--name">
        {/* The name is unknown until the server answers, and stays unknown if
            the transfer fails early; the URL's last segment is a better label
            than the whole address. */}
        <div className="task__namerow">
          <input
            type="checkbox"
            className="task__check"
            checked={selected}
            aria-label={`选择 ${task.name || task.url}`}
            title="选择此任务（可多选，对顶部按钮生效）"
            onClick={(e) => e.stopPropagation()}
            onChange={() => onSelect(task.id, true)}
          />
          <span className="task__name" title={task.name || task.url}>
            {task.name ? (
              ellipsizeMiddle(task.name, 60)
            ) : (
              <span className="task__name-pending">
                {ellipsizeMiddle(urlTail(task.url), 60)}
              </span>
            )}
          </span>
        </div>
        {(task.status === "failed" || task.status === "completed") &&
        task.error ? (
          <span className="task__error">{task.error}</span>
        ) : task.status === "downloading" && task.notice ? (
          <span className="task__url">{task.notice}</span>
        ) : (
          <span className="task__url" title={displayURL(task.url)}>
            {displayURL(task.url)}
          </span>
        )}
      </div>

      <div className="task__cell task__cell--center task__cell--status">
        <span
          className={`badge badge--${STATUS_TONE[task.status as TaskStatus] ?? "muted"}`}
        >
          {task.status === "completed" ? <IconCheck size={12} /> : null}
          {task.status === "failed" ? <IconAlert size={12} /> : null}
          {task.status === "downloading" ? <IconDownload size={12} /> : null}
          {STATUS_LABEL[task.status as TaskStatus] ?? task.status}
        </span>
      </div>

      <div className="task__cell task__cell--center task__meta">
        <span className="num task__meta-primary">
          {formatSize(task.size, task.status as TaskStatus)}
        </span>
        <span className="num">{formatWhen(task.createdAt)}</span>
      </div>

      <div className="task__cell task__cell--center task__meta">
        {PROGRESSED.includes(task.status as TaskStatus) ? (
          <>
            <div
              className={barClass}
              role="progressbar"
              aria-valuemin={0}
              aria-valuemax={100}
              // A completed row is full even when its verified size is 0; an
              // unfinished row with an unknown total stays indeterminate
              // rather than announcing a 0% it has not actually reached.
              aria-valuenow={
                completed || task.size > 0 ? Math.round(pct) : undefined
              }
              aria-label={`${task.name} 进度`}
            >
              <div className="progress__segment" style={{ width: `${pct}%` }} />
            </div>
            <span className="num task__meta-primary">
              {formatProgress(
                task.downloaded,
                task.size,
                task.status as TaskStatus,
              )}
            </span>
          </>
        ) : (
          <span className="num">{formatBytes(task.downloaded)}</span>
        )}
      </div>

      <div className="task__cell task__cell--center task__meta">
        {task.status === "downloading" ? (
          <>
            <span className="num task__meta-primary">{formatSpeed(task.speed)}</span>
            <span className="num">剩余 {formatEta(task.eta)}</span>
          </>
        ) : task.status === "completed" ? (
          // Segment counts are only meaningful while transferring; after the
          // fact the column would just repeat what the size column already
          // shows, so it is left blank.
          <span className="num">—</span>
        ) : task.segments > 1 ? (
          <span className="num">
            分片 {task.segmentsDone}/{task.segments}
          </span>
        ) : (
          <span className="num">—</span>
        )}
      </div>

      <div className="task__actions" onClick={(e) => e.stopPropagation()}>
        {task.status === "downloading" || task.status === "queued" ? (
          <button
            type="button"
            className="icon-btn"
            aria-label="暂停"
            title={task.status === "queued" ? "暂停（不再自动排队等待）" : "暂停"}
            onClick={() => void run(() => api.pauseTask(task.id))}
          >
            <IconPause size={16} />
          </button>
        ) : null}

        {task.status === "paused" ||
        task.status === "failed" ||
        task.status === "canceled" ||
        task.status === "queued" ? (
          <button
            type="button"
            className="icon-btn"
            aria-label="继续"
            title="继续"
            onClick={() => void run(() => api.resumeTask(task.id))}
          >
            <IconPlay size={16} />
          </button>
        ) : null}

        <button
          type="button"
          className="icon-btn"
          aria-label="打开所在目录"
          title="打开所在目录"
          onClick={() => void run(() => api.revealInExplorer(task.id))}
        >
          <IconFolder size={16} />
        </button>

        {/* The row trash asks first, exactly like the toolbar delete: one
            click removes the record, and "which click deletes what" should
            never be a question the user has to ask. */}
        <button
          type="button"
          className="icon-btn"
          aria-label="从列表移除"
          title="从列表移除（下好的文件不动，没下完的半成品会一起删）"
          onClick={() => setConfirmRemove(true)}
        >
          <IconTrash size={16} />
        </button>
      </div>

      {confirmRemove ? (
        // display:contents keeps this from becoming a seventh grid item, while
        // still swallowing the click: without it, choosing "移除" or "取消" in
        // the dialog also bubbles to the row's own onClick and toggles the
        // row's selection.
        <span
          style={{ display: "contents" }}
          onClick={(e) => e.stopPropagation()}
          onContextMenu={(e) => e.stopPropagation()}
        >
          <ConfirmDialog
            title="移除这条记录？"
            message={`将从列表里移除「${task.name || urlTail(task.url)}」。`}
            detail="只删除列表记录。下好的文件会保留；没下完的半成品和续传记录会一起删掉，同链接重下从头开始。"
            confirmLabel="移除"
            onConfirm={() => {
              setConfirmRemove(false);
              void run(() => api.removeTask(task.id, false));
            }}
            onCancel={() => setConfirmRemove(false)}
          />
        </span>
      ) : null}

      {menu ? (
        <RowContextMenu
          x={menu.x}
          y={menu.y}
          items={menuItems}
          onClose={closeMenu}
        />
      ) : null}
    </div>
  );
});
