import { useEffect, useState } from "react";
import type { JSX } from "react";
import { api } from "../api";

type Props = {
  version: string;
  /** The running version, shown only as context. */
  current?: string;
  onClose: () => void;
  /** Proceed with the actual self-update. */
  onUpdate: () => void;
};

/**
 * "What's new" before an update commits.
 *
 * The notes come from the GitHub release of that version, so the dialog shows
 * the same text the release page does. Fetching live instead of shipping them
 * in the binary is the only way a build can ever describe releases made after
 * it. A fetch that fails (offline, or a release published without notes) falls
 * back to a hint instead of blocking the update.
 */
export function UpdateNotesDialog({
  version,
  current,
  onClose,
  onUpdate,
}: Props): JSX.Element {
  const [notes, setNotes] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setNotes(null);
    setFailed(false);
    api
      .releaseNotes(version)
      .then((body) => {
        if (cancelled) return;
        if (body && body.trim() !== "") {
          setNotes(body);
        } else {
          setFailed(true);
        }
      })
      .catch(() => {
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
    };
  }, [version]);

  // Esc dismisses like the "later" button. Captured on the window and
  // stopped there: the app's global Escape handler also closes the settings
  // page behind this dialog, and without the stop one press would fold both
  // layers away at once.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.stopImmediatePropagation();
      onClose();
    };
    window.addEventListener("keydown", onKey, { capture: true });
    return () => window.removeEventListener("keydown", onKey, { capture: true });
  }, [onClose]);

  return (
    <div
      className="overlay"
      role="dialog"
      aria-modal="true"
      aria-label={`更新内容 v${version}`}
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="modal modal--notes">
        <div className="modal__head">
          <h2 className="modal__title">新版本 v{version}</h2>
        </div>

        <div className="modal__body">
          {notes === null && !failed ? (
            <p className="notes__hint">正在获取更新内容…</p>
          ) : null}
          {failed ? (
            <p className="notes__hint">
              更新内容没拉到（可能离线，或这个版本没写说明）。可以直接更新，也可以稍后去发布页看。
            </p>
          ) : null}
          {notes !== null ? <pre className="notes__body">{notes}</pre> : null}
          <p className="notes__hint">
            更新会先暂停全部任务，换好文件后自动重启，任务都在、可继续下载。
            {current ? `当前版本 v${current}。` : ""}
          </p>
        </div>

        <div className="modal__foot">
          <span className="modal__foot-spacer" />
          <button type="button" className="btn" onClick={onClose}>
            以后再说
          </button>
          <button
            type="button"
            className="btn btn--primary"
            autoFocus
            onClick={onUpdate}
          >
            立即更新
          </button>
        </div>
      </div>
    </div>
  );
}
