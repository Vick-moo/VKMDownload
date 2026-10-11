import type { JSX } from "react";

type Props = {
  title: string;
  /** One sentence naming the action and how many rows it affects. */
  message: string;
  /** The part that is easy to get wrong, such as what is *not* deleted. */
  detail?: string;
  confirmLabel: string;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
};

/**
 * A yes/no prompt for an action that cannot be undone.
 *
 * It exists for the bulk deletions, which are the only place in the app where a
 * single click can remove hundreds of rows. The detail line is deliberately
 * separate from the message: "the files on disk are not touched" is the thing a
 * user needs to read before deciding, and burying it in a longer sentence is how
 * it gets missed.
 */
export function ConfirmDialog({
  title,
  message,
  detail,
  confirmLabel,
  busy = false,
  onConfirm,
  onCancel,
}: Props): JSX.Element {
  return (
    <div className="overlay" role="dialog" aria-modal="true" aria-label={title}>
      <div className="modal modal--confirm">
        <div className="modal__head">
          <h2 className="modal__title">{title}</h2>
        </div>

        <div className="modal__body">
          <p className="confirm__message">{message}</p>
          {detail ? <p className="confirm__detail">{detail}</p> : null}
        </div>

        <div className="modal__foot">
          <span className="modal__foot-spacer" />
          <button type="button" className="btn" onClick={onCancel} disabled={busy}>
            取消
          </button>
          <button
            type="button"
            className="btn btn--danger"
            onClick={onConfirm}
            disabled={busy}
            autoFocus
          >
            {busy ? "处理中…" : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
