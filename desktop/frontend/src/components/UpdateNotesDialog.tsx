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

/** The release body is written for the GitHub page; this dialog only shows
 * the short version. The author marks where the in-app summary ends with an
 * HTML comment — invisible on GitHub — and everything past it (the backstory,
 * the asset table, the SmartScreen boilerplate) never reaches the dialog.
 * Markdown furniture is stripped so the summary reads as plain sentences, and
 * the hard cap is the backstop for bodies written before the marker existed. */
const APP_NOTES_END = "<!-- app-notes-end -->";
const APP_NOTES_MAX = 1200;

export function toDialogText(body: string): string {
  let text = body.includes(APP_NOTES_END)
    ? body.slice(0, body.indexOf(APP_NOTES_END))
    : body;
  text = text
    .split("\n")
    .map((line) => line.trim())
    // Tables and rules only read well rendered; as plain text the pipes are
    // noise.
    .filter((line) => line !== "" && !line.startsWith("|") && !/^-{3,}$/.test(line))
    .map((line) =>
      line
        .replace(/^#{1,6}\s*/, "")
        .replace(/\*\*/g, "")
        .replace(/`/g, "")
        // A pipe that survived the table-row filter is mid-sentence noise.
        .replace(/\|/g, " ")
        .replace(/ {2,}/g, " ")
        .trim(),
    )
    .join("\n");
  if (text.length > APP_NOTES_MAX) {
    const head = text.slice(0, APP_NOTES_MAX);
    const lastLine = head.lastIndexOf("\n");
    if (lastLine > 0) {
      // Cut on a line boundary, never mid-sentence.
      text = head.slice(0, lastLine);
    } else {
      // A single run with no line break to cut on. Prefer the last word
      // boundary so the text does not stop mid-word; failing that (one
      // enormous unbroken token) hard-cut with an ellipsis, so the reader can
      // see it was shortened instead of getting a silent, arbitrary slice.
      const tail = text.slice(0, APP_NOTES_MAX - 1);
      const wordEnd = tail.search(/\s+\S*$/);
      text = (wordEnd > 0 ? tail.slice(0, wordEnd) : tail) + "…";
    }
  }
  return text.trim();
}

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
        const summary = toDialogText(body);
        if (summary !== "") {
          setNotes(summary);
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
          {notes !== null ? (
            <div className="notes__body">
              {notes.split("\n").map((line, i) =>
                line.trim() === "" ? null : <p key={i}>{line}</p>,
              )}
            </div>
          ) : null}
          <p className="notes__hint">
            更新会先暂停全部任务，换好文件后自动重启，任务都在、可继续下载。
            {current ? `当前版本 v${current}。` : ""}
          </p>
        </div>

        <div className="modal__foot">
          <button
            type="button"
            className="btn btn--link"
            onClick={() =>
              void api
                .openReleasePage(
                  "https://github.com/Vick-moo/VKMDownload/releases/tag/v" + version,
                )
                .catch(() => {})
            }
          >
            完整说明在发布页
          </button>
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
