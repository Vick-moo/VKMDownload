/**
 * Shared types mirroring the Go side.
 *
 * The Go definitions live in desktop/manager.go and desktop/config.go; these
 * must stay in step with them. Wails generates bindings for the methods but
 * not for the struct shapes, so the shapes are declared here by hand.
 */

/** Lifecycle of a task, as reported by the manager. */
export type TaskStatus =
  | "queued"
  | "downloading"
  | "paused"
  | "completed"
  | "failed"
  | "canceled";

/** One transfer segment, used to draw the per-connection blocks. */
export interface SegmentView {
  index: number;
  start: number;
  end: number;
  written: number;
  done: boolean;
}

/** A task as the UI needs it. */
export interface TaskView {
  id: string;
  url: string;
  name: string;
  dest: string;
  size: number;
  downloaded: number;
  /** bytes/sec over the last reporting interval; 0 when stalled */
  speed: number;
  /** seconds remaining, 0 when unknown */
  eta: number;
  status: TaskStatus;
  /** user-facing message when status is "failed" */
  error: string;
  connections: number;
  segments: SegmentView[];
  createdAt: string;
  finishedAt: string;
}

/** Filter applied to the task list. */
export type Filter = "all" | "active" | "completed" | "failed";

/** User settings, persisted to disk by the Go side. */
export interface Config {
  /** start a task the moment it is created instead of leaving it queued */
  autoStart: boolean;
  downloadDir: string;
  connections: number;
  minSegmentSize: number;
  /** bytes/sec ceiling, 0 means unlimited */
  rateLimit: number;
  proxy: string;
  /** how many tasks may transfer at once; the rest queue */
  maxConcurrent: number;
  openFileWhenDone: boolean;
  openDirWhenDone: boolean;
  /** hide to the tray instead of exiting when the window is closed */
  minimizeToTray: boolean;
  clipboardWatch: boolean;
  clipboardAutoStart: boolean;
  clipboardConfirm: boolean;
  clipboardIntervalMs: number;
}

/** Progress event emitted by the Go side while a task transfers. */
export interface ProgressEvent {
  id: string;
  downloaded: number;
  total: number;
  speed: number;
  eta: number;
  segments: SegmentView[];
  connections: number;
}

/** Snapshots the top bar renders. */
export interface GlobalStats {
  activeCount: number;
  totalSpeed: number;
}
