import { useEffect, useState } from "react";
import type { JSX } from "react";
import { api } from "../api";
import type { Config } from "../types";
import { IconFolder } from "./Icons";

interface Props {
  config: Config | null;
  /** From the binding; shown at the foot of the page. */
  version: string;
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
export function SettingsPage({
  config,
  version,
  onSaved,
}: Props): JSX.Element {
  const [draft, setDraft] = useState<Config | null>(config);
  const [status, setStatus] = useState("");
  // Holds the outcome of the clipboard test button.
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
            <span className="setting__title">全局速度上限</span>
            <span className="setting__hint">
              所有任务加起来的下载速度上限，单位 KB/s。填 0 表示不限速。
            </span>
          </span>
          <input
            type="number"
            min={0}
            max={10485760}
            step={64}
            style={{ width: 110 }}
            value={draft.speedLimitKb}
            onChange={(e) =>
              patch({ speedLimitKb: Math.max(0, Number(e.target.value) || 0) })
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
        <h3 className="settings-section__title">计划下载</h3>

        <p className="settings-section__note">
          工具栏上的「计划下载」会开始全部任务，等它们都结束后执行下面的动作。
          同时下载的数量由上面的「同时下载的任务数」决定。
        </p>

        <Switch
          title="完成后播放提示音"
          hint="两声升调提示音，不需要额外音频文件。"
          checked={draft.planSound}
          onChange={(v) => patch({ planSound: v })}
        />

        <Switch
          title="完成后自动关机"
          hint="有 60 秒缓冲，期间运行 shutdown /a 可以取消。"
          checked={draft.planShutdown}
          onChange={(v) => patch({ planShutdown: v })}
        />
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

      <section className="settings-section">
        <h3 className="settings-section__title">浏览器扩展</h3>

        <p className="settings-section__note">
          装好扩展后，浏览器里的下载会交给 VKMDownload。扩展每次都会先问这里该不该接管，
          所以下面的改动立刻生效，不用重装扩展。
        </p>

        <Switch
          title="接管浏览器下载"
          hint="总开关。关闭后浏览器恢复自己下载，扩展仍然装着但不再拦截。"
          checked={draft.extensionEnabled}
          onChange={(v) => patch({ extensionEnabled: v })}
        />

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">小于此大小交给浏览器</span>
            <span className="setting__hint">
              浏览器自己也会下载很多小东西（自动更新、网页图标、PDF 里的图片）。
              设一个下限可以把这些挡在外面。填 0 表示全部接管。
            </span>
          </span>
          <span className="field__row" style={{ flex: "0 0 auto" }}>
            <input
              type="number"
              min={0}
              max={1024}
              step={1}
              style={{ width: 88 }}
              value={Math.round(draft.extensionMinSizeKb / 1024)}
              onChange={(e) =>
                patch({ extensionMinSizeKb: Math.max(0, Number(e.target.value) || 0) * 1024 })
              }
              disabled={!draft.extensionEnabled}
            />
            <span className="field__suffix">MB</span>
          </span>
        </label>

        <label className="setting setting--stacked">
          <span className="setting__text">
            <span className="setting__title">不接管的网站</span>
            <span className="setting__hint">
              一行一个域名，子域名也算。例如填 <code>example.com</code> 会同时排除
              <code>www.example.com</code> 和 <code>cdn.example.com</code>。
            </span>
          </span>
          <textarea
            className="field__textarea"
            rows={3}
            spellCheck={false}
            placeholder={"example.com\nanother-site.org"}
            value={draft.extensionSkipHosts}
            onChange={(e) => patch({ extensionSkipHosts: e.target.value })}
            disabled={!draft.extensionEnabled}
          />
        </label>
      </section>

      <div className="settings__foot">
        {/* The status comes first so its `margin-right: auto` pushes the button
            to the right edge. With the button first, the auto margin sat after
            the last item and dragged both of them left — which only became
            visible once a save produced the status, making the button appear to
            jump across the bar. */}
        {status ? <span className="settings__status">{status}</span> : null}
        <button type="button" className="btn btn--primary" onClick={save}>
          保存设置
        </button>
      </div>

      {/* The version lived in a top bar that the brand row replaced, and it went
          with the bar rather than moving. It belongs here: the first thing
          anyone reporting a problem is asked for is this number. */}
      {version ? (
        <p className="settings__version">VKMDownload v{version}</p>
      ) : null}
    </div>
  );
}
