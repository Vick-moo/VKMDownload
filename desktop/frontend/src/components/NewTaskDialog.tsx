import { useEffect, useRef, useState } from "react";
import type { JSX } from "react";
import { api } from "../api";
import type { Config } from "../types";
import { IconFolder, IconX } from "./Icons";

interface Props {
  open: boolean;
  /** Pre-filled URL, used when a link is captured from the clipboard. */
  initialURL?: string;
  /** Pre-filled filename from the ask-location flow (extension suggestion). */
  initialFilename?: string;
  /** Carried through to the created task; some hosts require it. */
  initialReferer?: string;
  /** True when opened to place an intercepted download: closing abandons it,
      and the browser-hint below applies. */
  fromAsk?: boolean;
  config: Config | null;
  onClose: () => void;
  /** Called after a task is created, with whether it was started. */
  onCreated: () => void;
}

/**
 * NewTaskDialog collects everything needed to create a download.
 *
 * The two buttons map to the two intentions a user actually has: "开始下载"
 * transfers now, "只加入列表" queues it for later. That distinction is clearer
 * than a checkbox, because it says what pressing the button will do.
 */
export function NewTaskDialog({
  open,
  initialURL,
  initialFilename,
  initialReferer,
  fromAsk,
  config,
  onClose,
  onCreated,
}: Props): JSX.Element | null {
  const [url, setURL] = useState("");
  const [dest, setDest] = useState("");
  const [filename, setFilename] = useState("");
  const [referer, setReferer] = useState("");
  const [conns, setConns] = useState(8);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const urlRef = useRef<HTMLInputElement>(null);

  // Reset the form each time the dialog opens, so a previous attempt's error
  // or half-typed URL does not greet the user.
  useEffect(() => {
    if (!open) return;
    setURL(initialURL ?? "");
    setError("");
    setFilename(initialFilename ?? "");
    setReferer(initialReferer ?? "");
    setBusy(false);
    setDest(config?.downloadDir ?? "");
    setConns(config?.connections ?? 8);
    // Focus lands on the field the user most likely wants.
    window.setTimeout(() => urlRef.current?.focus(), 30);
  }, [open, initialURL, config]);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open) return null;

  async function submit(start: boolean) {
    const trimmed = url.trim();
    if (!trimmed) {
      setError("请填写下载链接");
      return;
    }
    // Same rule as the backend: only http(s) reaches the engine. Checking here
    // fails fast at the dialog instead of round-tripping through Go.
    if (!/^https?:\/\//i.test(trimmed)) {
      setError("链接必须以 http:// 或 https:// 开头");
      return;
    }
    setBusy(true);
    setError("");
    try {
      await api.newTask({
        url: trimmed,
        dest: dest.trim(),
        filename: filename.trim(),
        connections: conns,
        referer,
        start,
      } as never);
      onCreated();
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  async function browse() {
    try {
      const picked = await api.pickDirectory();
      if (picked) setDest(picked);
    } catch {
      // A cancelled dialog is not an error worth reporting.
    }
  }

  return (
    <div className="overlay" role="dialog" aria-modal="true" aria-label="新建下载">
      <div className="modal">
        <header className="modal__head">
          <h2 className="modal__title">新建下载</h2>
          <button
            type="button"
            className="icon-btn"
            aria-label="关闭"
            onClick={onClose}
          >
            <IconX size={16} />
          </button>
        </header>

        <div className="modal__body">
          <label className="field">
            <span className="field__label">下载链接</span>
            <input
              ref={urlRef}
              type="text"
              value={url}
              spellCheck={false}
              autoComplete="off"
              placeholder="https://example.com/file.zip"
              onChange={(e) => setURL(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !busy) void submit(true);
              }}
            />
          </label>

          <label className="field">
            <span className="field__label">保存目录</span>
            <div className="field__row">
              <input
                type="text"
                value={dest}
                spellCheck={false}
                onChange={(e) => setDest(e.target.value)}
              />
              <button type="button" className="btn" onClick={browse}>
                <IconFolder size={16} />
                浏览
              </button>
            </div>
          </label>

          <div className="field-grid">
            <label className="field">
              <span className="field__label">文件名（可留空）</span>
              <input
                type="text"
                value={filename}
                spellCheck={false}
                placeholder="留空则由服务器决定"
                onChange={(e) => setFilename(e.target.value)}
              />
            </label>
            <label className="field">
              <span className="field__label">连接数</span>
              <input
                type="number"
                min={1}
                max={64}
                value={conns}
                onChange={(e) => setConns(Number(e.target.value) || 1)}
              />
            </label>
          </div>

          {fromAsk ? (
            <p className="field__hint">
              浏览器里拦截到的下载，请确认保存位置后开始；关闭此窗口则放弃这次下载。
              若浏览器开了“下载前询问保存位置”，请关掉，否则同一个文件会弹两次。
            </p>
          ) : null}
          {error ? <p className="field__error">{error}</p> : null}
        </div>

        <footer className="modal__foot">
          <button type="button" className="btn" onClick={onClose} disabled={busy}>
            取消
          </button>
          <span className="modal__foot-spacer" />
          <button
            type="button"
            className="btn"
            disabled={busy}
            onClick={() => void submit(false)}
          >
            只加入列表
          </button>
          <button
            type="button"
            className="btn btn--primary"
            disabled={busy}
            onClick={() => void submit(true)}
          >
            {busy ? "创建中…" : "开始下载"}
          </button>
        </footer>
      </div>
    </div>
  );
}
