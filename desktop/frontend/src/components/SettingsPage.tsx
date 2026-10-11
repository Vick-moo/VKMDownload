import { useEffect, useState } from "react";
import type { JSX } from "react";
import { api } from "../api";
import type { Config } from "../types";
import { IconFolder } from "./Icons";

interface Props {
  config: Config | null;
  onSaved: (cfg: Config) => void;
}

/** A labelled switch row. */
function Switch({
  title,
  hint,
  checked,
  disabled,
  nested,
  onChange,
}: {
  title: string;
  hint?: string;
  checked: boolean;
  disabled?: boolean;
  nested?: boolean;
  onChange: (v: boolean) => void;
}): JSX.Element {
  return (
    <label className={`setting${nested ? " setting--nested" : ""}`}>
      <span className="setting__text">
        <span className="setting__title">{title}</span>
        {hint ? <span className="setting__hint">{hint}</span> : null}
      </span>
      <span className="switch">
        <input
          type="checkbox"
          checked={checked}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked)}
        />
        <span className="switch__track" />
        <span className="switch__thumb" />
      </span>
    </label>
  );
}

/**
 * SettingsPage edits every option.
 *
 * Each behaviour has its own switch rather than being bundled with a related
 * one, because the useful combinations are not the ones a designer would guess:
 * capturing links without downloading them is a real workflow, and so is
 * downloading without being asked.
 */
export function SettingsPage({ config, onSaved }: Props): JSX.Element {
  const [draft, setDraft] = useState<Config | null>(config);
  const [status, setStatus] = useState("");
  const [probe, setProbe] = useState("");

  useEffect(() => {
    setDraft(config);
  }, [config]);

  if (!draft) {
    return (
      <div className="empty">
        <p className="empty__hint">正在读取设置…</p>
      </div>
    );
  }

  /** Applies a partial change to the draft. */
  function patch(p: Partial<Config>) {
    setDraft((d) => (d ? { ...d, ...p } : d));
  }

  async function save() {
    if (!draft) return;
    try {
      await api.setConfig(draft as never);
      onSaved(draft);
      setStatus("已保存");
      window.setTimeout(() => setStatus(""), 1600);
    } catch (e) {
      setStatus(e instanceof Error ? e.message : String(e));
    }
  }

  async function browse() {
    try {
      const picked = await api.pickDirectory();
      if (picked) patch({ downloadDir: picked });
    } catch {
      // Cancelling the dialog is not an error.
    }
  }

  async function testClipboard() {
    try {
      const res = await api.clipboardProbe();
      const url = res["url"] ?? "";
      setProbe(
        url
          ? `识别到链接：${url.slice(0, 72)}`
          : res["text"]
            ? "剪贴板里没有可识别的 http/https 链接"
            : "剪贴板是空的",
      );
    } catch (e) {
      setProbe(e instanceof Error ? e.message : String(e));
    }
  }

  return (
    <div className="settings">
      <section className="settings-section">
        <h3 className="settings-section__title">下载</h3>

        <Switch
          title="自动开始下载"
          hint="开启后，创建任务立刻开始下载；关闭则只加入列表等你手动开始。"
          checked={draft.autoStart}
          onChange={(v) => patch({ autoStart: v })}
        />

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">默认保存目录</span>
            <span className="setting__hint">{draft.downloadDir || "未设置"}</span>
          </span>
          <span className="field__row" style={{ flex: "0 0 auto" }}>
            <button type="button" className="btn" onClick={browse}>
              <IconFolder size={16} />
              浏览
            </button>
          </span>
        </label>

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">默认连接数</span>
            <span className="setting__hint">
              单个文件的并行连接数。8 适合大多数服务器；遇到限速的站点可以调高。
            </span>
          </span>
          <input
            type="number"
            min={1}
            max={64}
            style={{ width: 88 }}
            value={draft.connections}
            onChange={(e) => patch({ connections: Number(e.target.value) || 1 })}
          />
        </label>

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">同时下载的任务数</span>
            <span className="setting__hint">
              超出的任务会排队。数值越大越容易占满带宽。
            </span>
          </span>
          <input
            type="number"
            min={1}
            max={16}
            style={{ width: 88 }}
            value={draft.maxConcurrent}
            onChange={(e) =>
              patch({ maxConcurrent: Number(e.target.value) || 1 })
            }
          />
        </label>

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">代理</span>
            <span className="setting__hint">
              留空则跟随系统环境变量（HTTP_PROXY / HTTPS_PROXY）。
            </span>
          </span>
          <input
            type="text"
            placeholder="http://127.0.0.1:7890"
            spellCheck={false}
            style={{ width: 200 }}
            value={draft.proxy}
            onChange={(e) => patch({ proxy: e.target.value })}
          />
        </label>
      </section>

      <section className="settings-section">
        <h3 className="settings-section__title">剪贴板</h3>

        <Switch
          title="监控剪贴板链接"
          hint="复制一个下载链接后自动识别。关闭时不做任何后台轮询。"
          checked={draft.clipboardWatch}
          onChange={(v) => patch({ clipboardWatch: v })}
        />

        <Switch
          title="识别到链接时直接开始下载"
          hint="开启后不再询问，复制完链接就开始下。"
          checked={draft.clipboardAutoStart}
          disabled={!draft.clipboardWatch}
          nested
          onChange={(v) => patch({ clipboardAutoStart: v })}
        />

        <Switch
          title="识别到链接时先弹窗确认"
          hint="开启后会先问你要不要下载。与上一项同时开启时，以弹窗为准。"
          checked={draft.clipboardConfirm}
          disabled={!draft.clipboardWatch}
          nested
          onChange={(v) => patch({ clipboardConfirm: v })}
        />

        <Switch
          title="只加入列表，不自动开始"
          hint="复制链接后任务进入列表并保持排队，由你决定何时开始。"
          checked={draft.clipboardWatch && !draft.clipboardAutoStart}
          disabled={!draft.clipboardWatch}
          nested
          onChange={() => patch({ clipboardAutoStart: false })}
        />

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">检测间隔</span>
            <span className="setting__hint">
              越短响应越快，但会稍微多占用一点 CPU。默认 800 毫秒。
            </span>
          </span>
          <input
            type="number"
            min={200}
            max={10000}
            step={100}
            style={{ width: 96 }}
            value={draft.clipboardIntervalMs}
            disabled={!draft.clipboardWatch}
            onChange={(e) =>
              patch({ clipboardIntervalMs: Number(e.target.value) || 800 })
            }
          />
        </label>

        <div className="setting">
          <span className="setting__text">
            <span className="setting__title">测试剪贴板识别</span>
            <span className="setting__hint">{probe || "先复制一个链接，再点右边按钮。"}</span>
          </span>
          <button type="button" className="btn" onClick={testClipboard}>
            测试
          </button>
        </div>
      </section>

      <section className="settings-section">
        <h3 className="settings-section__title">完成后</h3>

        <Switch
          title="下载完成后打开文件"
          hint="用系统默认程序打开刚下好的文件。"
          checked={draft.openFileWhenDone}
          onChange={(v) => patch({ openFileWhenDone: v })}
        />
        <Switch
          title="下载完成后打开所在目录"
          hint="在文件资源管理器中定位到该文件。"
          checked={draft.openDirWhenDone}
          onChange={(v) => patch({ openDirWhenDone: v })}
        />
      </section>

      <section className="settings-section">
        <h3 className="settings-section__title">窗口</h3>

        <Switch
          title="关闭窗口时最小化到托盘"
          hint="下载还在进行时不会退出，避免中断传输。"
          checked={draft.minimizeToTray}
          onChange={(v) => patch({ minimizeToTray: v })}
        />
      </section>

      <div className="settings__foot">
        <button type="button" className="btn btn--primary" onClick={save}>
          保存设置
        </button>
        {status ? <span className="settings__status">{status}</span> : null}
      </div>
    </div>
  );
}
