import { memo, useState } from "react";
import type { JSX } from "react";
import { api } from "../api";
import {
  formatBytes,
  formatEta,
  formatPercent,
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
  onChanged: () => void;
  /** Reports a failed row action to the toolbar notice line. */
  onError: (message: string) => void;
}

/** Statuses that carry a progress bar worth showing. */
const PROGRESSED: TaskStatus[] = ["downloading", "paused", "completed", "failed"];

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
  onChanged,
  onError,
}: Props): JSX.Element {
  const [confirmRemove, setConfirmRemove] = useState(false);
  const pct =
    task.size > 0 ? Math.min(100, (task.downloaded / task.size) * 100) : 0;

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

  return (
    <div
      className={`task${selected ? " task--selected" : ""}${
        task.status === "failed" ? " task--failed" : ""
      }`}
      onClick={(e) => onSelect(task.id, e.ctrlKey || e.metaKey)}
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
        <span className="num task__meta-primary">{formatBytes(task.size)}</span>
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
              aria-valuenow={Math.round(pct)}
              aria-label={`${task.name} 进度`}
            >
              <div className="progress__segment" style={{ width: `${pct}%` }} />
            </div>
            <span className="num task__meta-primary">
              {formatPercent(task.downloaded, task.size)}
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
    </div>
  );
});
