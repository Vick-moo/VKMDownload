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
import { STATUS_LABEL, STATUS_TONE } from "../types";
import type { TaskStatus, TaskView } from "../types";
import {
  IconAlert,
  IconCheck,
  IconDownload,
  IconFolder,
  IconPause,
  IconPlay,
  IconTrash,
  IconX,
} from "./Icons";

interface Props {
  task: TaskView;
  selected: boolean;
  onSelect: (id: string, additive: boolean) => void;
  onChanged: () => void;
}

/** Statuses that carry a progress bar worth showing. */
const PROGRESSED: TaskStatus[] = ["downloading", "paused", "completed", "failed"];

/**
 * TaskRow renders one download.
 *
 * Three lines: the name and its status, the progress bar, then the numbers.
 * Keeping the height fixed means the list does not reflow as speeds and ETAs
 * change every few hundred milliseconds.
 */
export function TaskRow({
  task,
  selected,
  onSelect,
  onChanged,
}: Props): JSX.Element {
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
      // Surfacing this in the row would fight with the row's own status, so it
      // goes to the console; the task status already reflects the failure.
      console.error(e);
    }
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
        <span className="task__name" title={task.name}>
          {ellipsizeMiddle(task.name, 60)}
        </span>
        {task.status === "failed" && task.error ? (
          <span className="task__error">{task.error}</span>
        ) : (
          <span className="task__url" title={task.url}>
            {task.url}
          </span>
        )}
      </div>

      <div className="task__cell task__cell--status">
        <span
          className={`badge badge--${STATUS_TONE[task.status as TaskStatus] ?? "muted"}`}
        >
          {task.status === "completed" ? <IconCheck size={12} /> : null}
          {task.status === "failed" ? <IconAlert size={12} /> : null}
          {task.status === "downloading" ? <IconDownload size={12} /> : null}
          {STATUS_LABEL[task.status as TaskStatus] ?? task.status}
        </span>
      </div>

      <div className="task__cell task__cell--right task__meta">
        <span className="num task__meta-primary">{formatBytes(task.size)}</span>
        <span className="num">{formatWhen(task.createdAt)}</span>
      </div>

      <div className="task__cell task__cell--right task__meta">
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

      <div className="task__cell task__cell--right task__meta">
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
            title="暂停"
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

        {task.status !== "completed" ? (
          <button
            type="button"
            className="icon-btn"
            aria-label="取消并清除断点"
            title="取消并清除断点"
            onClick={() => void run(() => api.cancelTask(task.id))}
          >
            <IconX size={16} />
          </button>
        ) : null}

        <button
          type="button"
          className="icon-btn"
          aria-label="删除任务"
          title="删除任务"
          onClick={() => void run(() => api.removeTask(task.id, false))}
        >
          <IconTrash size={16} />
        </button>
      </div>
    </div>
  );
}
