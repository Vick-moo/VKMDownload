import { useEffect, useRef, useState } from "react";
import type { JSX } from "react";
import { api } from "../api";
import type { Config } from "../types";
import { IconFolder, IconX } from "./Icons";

/**
 * Longest link the dialog accepts.
 *
 * This is a typo guard, not a protocol limit: a pasted-in blob (a whole web
 * page, a chat log) is almost never a link. Real URLs stop far short of it.
 */
const MAX_URL_LENGTH = 8192;

interface Props {
  open: boolean;
  /** Pre-filled URL, used when a link is captured from the clipboard. */
  initialURL?: string;
  /** Pre-filled filename from the ask-location flow (extension suggestion). */
  initialFilename?: string;
  /** Carried through to the created task; some hosts require it. */
  initialReferer?: string;
  /** Browser cookies for the URL, replayed by the engine; never shown. */
  initialCookie?: string;
  /** Browser UA, replayed when the engine's default is refused. */
  initialUserAgent?: string;
  /** Browser-known total size in bytes; 0 means unknown. */
  initialSize?: number;
  /** True when opened to place an intercepted download: closing abandons it,
      and the browser-hint below applies. */
  fromAsk?: boolean;
  config: Config | null;
  onClose: () => void;
  /** Called after a task is created, with whether it was started. */
  onCreated: () => void;
  /** Called with the existing task id when the URL is already listed. */
  onLocateTask?: (id: string) => void;
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
  initialCookie,
  initialUserAgent,
  initialSize,
  fromAsk,
  config,
  onClose,
  onCreated,
  onLocateTask,
}: Props): JSX.Element | null {
  const [url, setURL] = useState("");
  const [dest, setDest] = useState("");
  const [filename, setFilename] = useState("");
  const [referer, setReferer] = useState("");
  const [cookie, setCookie] = useState("");
  const [userAgent, setUserAgent] = useState("");
  const [conns, setConns] = useState(8);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const urlRef = useRef<HTMLInputElement>(null);
  // Whether the user has already touched the destination. The type-based
  // default arrives from the backend asynchronously and must not overwrite a
  // folder the user typed or picked while that call was in flight.
  const destTouched = useRef(false);

  // Reset the form each time the dialog opens, so a previous attempt's error
  // or half-typed URL does not greet the user.
  useEffect(() => {
    if (!open) return;
    setURL(initialURL ?? "");
    setError("");
    setFilename(initialFilename ?? "");
    setReferer(initialReferer ?? "");
    setCookie(initialCookie ?? "");
    setUserAgent(initialUserAgent ?? "");
    setBusy(false);
    destTouched.current = false;
    // The download folder is the immediate value, so the field is never empty
    // while the type-based lookup below is in flight.
    setDest(config?.downloadDir ?? "");
    // With sorting on, pre-fill the group folder for this file type. The rule
    // lives in Go (organizeTarget), so the dialog cannot drift from where the
    // organizer would have put an automatic download. It matters because a
    // task created here is Manual: the organizer will not move it later, so
    // the folder has to be right now. An empty name or an unmatched extension
    // leaves the download folder in place, which is the same fallback Go uses.
    let cancelled = false;
    if (config?.organizeEnabled) {
      void api
        .defaultDestFor((initialFilename ?? "").trim())
        .then((dir) => {
          if (!cancelled && !destTouched.current && dir) setDest(dir);
        })
        .catch(() => {
          // Keep the download folder: a failed lookup is not worth an error.
        });
    }
    setConns(config?.connections ?? 8);
    // Focus lands on the field the user most likely wants. The timer is
    // cleared on close/teardown so a late focus cannot land on a field that
    // has already been unmounted.
    const focusTimer = window.setTimeout(() => urlRef.current?.focus(), 30);
    return () => {
      cancelled = true;
      window.clearTimeout(focusTimer);
    };
  }, [open, initialURL, initialFilename, initialReferer, initialCookie, initialUserAgent, initialSize, fromAsk, config]);

  // Esc dismisses the dialog. Captured and stopped on the window, like the
  // confirm and update-notes dialogs: the app's global Escape handler also
  // closes the settings page behind this one (Ctrl+N can open the dialog over
  // settings), and without the stop one press would fold both layers away.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.stopImmediatePropagation();
      onClose();
    };
    window.addEventListener("keydown", onKey, { capture: true });
    return () => window.removeEventListener("keydown", onKey, { capture: true });
  }, [open, onClose]);

  if (!open) return null;

  async function submit(start: boolean) {
    const trimmed = url.trim();
    if (!trimmed) {
      setError("请填写下载链接");
      return;
    }
    if (trimmed.length > MAX_URL_LENGTH) {
      setError(`链接过长（超过 ${MAX_URL_LENGTH} 个字符），请检查是否粘贴了多余内容`);
      return;
    }
    // Same rule as the backend: only http(s) reaches the engine. Checking here
    // fails fast at the dialog instead of round-tripping through Go. Parsing
    // once rejects malformed strings a prefix-only regex let through — a URL
    // with spaces in the host, or one with nothing after the scheme.
    let parsed: URL;
    try {
      parsed = new URL(trimmed);
    } catch {
      setError("链接格式不正确，请检查后重试");
      return;
    }
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
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
        cookie,
        userAgent,
        size: initialSize ?? 0,
        start,
      } as never);
      onCreated();
      onClose();
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      // A duplicate URL is refused by the backend; point at the existing
      // record instead of leaving the user staring at an error.
      if (msg.includes("已在列表中")) {
        try {
          const id = await api.findUnfinishedByURL(trimmed);
          if (id) {
            onClose();
            onLocateTask?.(id);
            return;
          }
        } catch {
          // Fall through to the plain error below.
        }
      }
      setError(msg);
    } finally {
      setBusy(false);
    }
  }

  async function browse() {
    try {
      const picked = await api.pickDirectory();
      if (picked) {
        destTouched.current = true;
        setDest(picked);
      }
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
                onChange={(e) => {
                  // The user's own choice always wins over the pre-filled
                  // type folder.
                  destTouched.current = true;
                  setDest(e.target.value);
                }}
              />
              <button type="button" className="btn" onClick={browse}>
                <IconFolder size={16} />
                浏览
              </button>
            </div>
          </label>

          {config?.organizeEnabled ? (
            <p className="field__hint">
              已按文件类型预填；你改了就按你的位置，下载完成后不会再移动。
            </p>
          ) : null}

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
