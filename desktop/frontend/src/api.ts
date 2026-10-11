/**
 * Thin wrapper over the generated Wails bindings.
 *
 * Components import from here rather than from wailsjs directly, so the bridge
 * surface is visible in one file and the event names are not scattered as
 * string literals.
 */
import {
  CancelTask,
  ClipboardProbe,
  Config as GetConfig,
  ListTasks,
  NewTask,
  OpenFile,
  PauseTask,
  PickDirectory,
  RemoveTask,
  ResumeTask,
  RevealInExplorer,
  SetConfig,
  TaskStats,
  Version,
} from "../wailsjs/go/main/App";
import { EventsOn } from "../wailsjs/runtime/runtime";
import type { Config, StartRequest, TaskView } from "./types";
export const api = {
  version: (): Promise<string> => Version(),
  listTasks: (): Promise<TaskView[]> => ListTasks(),
  newTask: (req: StartRequest): Promise<TaskView> => NewTask(req),
  pauseTask: (id: string): Promise<void> => PauseTask(id),
  resumeTask: (id: string): Promise<void> => ResumeTask(id),
  cancelTask: (id: string): Promise<void> => CancelTask(id),
  removeTask: (id: string, deleteFiles: boolean): Promise<void> =>
    RemoveTask(id, deleteFiles),
  revealInExplorer: (id: string): Promise<void> => RevealInExplorer(id),
  openFile: (id: string): Promise<void> => OpenFile(id),
  stats: (): Promise<Record<string, number>> => TaskStats(),
  getConfig: (): Promise<Config> => GetConfig(),
  setConfig: (cfg: Config): Promise<void> => SetConfig(cfg),
  pickDirectory: (): Promise<string> => PickDirectory(),
  clipboardProbe: (): Promise<Record<string, string>> => ClipboardProbe(),
};

/** Event names emitted by the Go side. */
export const EVENTS = {
  added: "task:added",
  progress: "task:progress",
  done: "task:done",
  failed: "task:failed",
  removed: "task:removed",
  clipboardURL: "clipboard:url",
  filesDropped: "files:dropped",
} as const;

/**
 * Subscribes to a bridge event.
 *
 * The callback is wrapped so the payload arrives typed, and the return value is
 * normalised to a plain function: Wails hands back its own cancel type, which
 * differs between versions.
 */
export function on<T>(event: string, handler: (payload: T) => void): () => void {
  const cancel = EventsOn(event, (payload: T) => handler(payload));
  return () => {
    if (typeof cancel === "function") cancel();
  };
}
