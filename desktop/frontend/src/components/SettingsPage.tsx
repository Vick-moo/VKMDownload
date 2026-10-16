import { useEffect, useRef, useState } from "react";
import type { JSX } from "react";
import { main } from "../../wailsjs/go/models";
import { api } from "../api";
import type { Config, OrganizeGroup, UpdateStatus } from "../types";
import { IconFolder } from "./Icons";

interface Props {
  config: Config | null;
  /** From the binding; shown at the foot of the page. */
  version: string;
  onSaved: (cfg: Config) => void;
}

/**
 * The editable shape. `apiAuthRequired` exists on the Go Config but the
 * generated TS model has not been regenerated yet, so it is declared here so
 * the switch survives until save and travels with the SetConfig payload.
 */
type Draft = Config & { verifyPeerProcess?: boolean };

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
  const [draft, setDraft] = useState<Draft | null>(config);
  const [status, setStatus] = useState("");
  // Short-lived notices (radio changes, copy confirmations) share the status
  // line with the save confirmation.
  const noteTimer = useRef<number | null>(null);
  function note(msg: string) {
    setStatus(msg);
    if (noteTimer.current !== null) window.clearTimeout(noteTimer.current);
    noteTimer.current = window.setTimeout(() => setStatus(""), 2600);
  }
  // Tabbed categories, Chrome-tabs style: one concern per tab instead of a
  // long scroll. Sections stay mounted (hidden) so draft edits and the
  // update poller survive switching.
  const [tab, setTab] = useState("下载");
  // Holds the outcome of the clipboard test button.
  const [probe, setProbe] = useState("");
  // Self-update state, polled while a download is in flight.
  const [upd, setUpd] = useState<UpdateStatus | null>(null);
  const [updMsg, setUpdMsg] = useState("");
  const [updBusy, setUpdBusy] = useState(false);
  const pollTimer = useRef<number | null>(null);


  useEffect(() => {
    setDraft(config);
  }, [config]);

  // The update state lives in the backend: leaving this page unmounts it, so
  // re-entering must restore whatever the backend already knows instead of
  // forcing another download of the same file.
  useEffect(() => {
    let cancelled = false;
    void api
      .updateProgress()
      .then((s) => {
        if (cancelled) return;
        if (s.state !== "idle") {
          setUpd(s);
          if (s.state === "downloading") pollUpdate();
        }
      })
      .catch(() => {
        // Offline is not an error worth surfacing.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(
    () => () => {
      if (pollTimer.current !== null) window.clearTimeout(pollTimer.current);
      if (noteTimer.current !== null) window.clearTimeout(noteTimer.current);
    },
    [],
  );

  function pollUpdate() {
    if (pollTimer.current !== null) window.clearTimeout(pollTimer.current);
    pollTimer.current = window.setTimeout(async () => {
      try {
        const s = await api.updateProgress();
        setUpd(s);
        if (s.state === "downloading") pollUpdate();
      } catch {
        // A failed poll is not worth surfacing; the next one retries.
      }
    }, 800);
  }

  async function checkNow() {
    setUpdBusy(true);
    setUpdMsg("");
    try {
      const info = await api.checkUpdate();
      setUpdMsg(
        info.available
          ? `发现新版本 ${info.version}，可以一键更新。`
          : "已是最新版本。",
      );
      if (info.available) {
        setUpd({
          state: "idle",
          version: info.version,
          downloaded: 0,
          total: 0,
          error: "",
        });
      }
    } catch (e) {
      setUpdMsg(`检查失败：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setUpdBusy(false);
    }
  }

  async function startDl() {
    setUpdBusy(true);
    setUpdMsg("");
    try {
      const s = await api.startUpdate();
      setUpd(s);
      if (s.state === "downloading") pollUpdate();
    } catch (e) {
      setUpdMsg(`更新失败：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setUpdBusy(false);
    }
  }

  async function applyUpd() {
    setUpdBusy(true);
    try {
      await api.applyUpdate();
      // The app quits for the swap; if we are still here it failed.
      setUpdMsg("应用更新失败，程序还在运行。");
    } catch (e) {
      setUpdMsg(`更新失败：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setUpdBusy(false);
    }
  }

  /** Where a group's files land: an absolute Dir wins, otherwise the organize root/Name. */
  function groupPlace(organizeDir: string, g: OrganizeGroup): string {
    const dir = (g.dir || "").trim();
    if (dir) return dir;
    const name = (g.name || "").trim();
    if (!name) return "";
    return `${(organizeDir || "").replace(/[/\\]+$/, "")}\\${name}`;
  }

  async function pickGroupDir(i: number) {
    try {
      const picked = await api.pickDirectory();
      if (!picked || !draft) return;
      const base =
        picked
          .split(/[\\/]/)
          .filter((s) => s !== "")
          .pop() || "";
      const groups: OrganizeGroup[] = draft.organizeGroups.slice();
      const cur = groups[i];
      groups[i] = { ...cur, dir: picked, name: base || cur.name } as OrganizeGroup;
      patch({ organizeGroups: groups });
    } catch {
      // A cancelled dialog is not an error worth reporting.
    }
  }

  function updLine(): string {   if (updMsg) return updMsg;
    if (!upd) return "自动检查新版本，有更新时顶部会出现横幅。";
    if (upd.state === "downloading") {
      const pct =
        upd.total > 0 ? ` ${Math.floor((upd.downloaded / upd.total) * 100)}%` : "";
      return `正在下载新版${upd.version}${pct}…`;
    }
    if (upd.state === "ready") return `新版 ${upd.version} 已下好，重启即生效。`;
    if (upd.state === "failed" && upd.error) return `更新失败：${upd.error}`;
    return "自动检查新版本，有更新时顶部会出现横幅。";
  }

  if (!draft) {
    return (
      <div className="empty">
        <p className="empty__hint">正在读取设置…</p>
      </div>
    );
  }

  const TABS = ["下载", "浏览器扩展", "计划下载", "剪贴板", "完成后", "窗口", "软件更新"];

  /** Applies a partial change to the draft. */
  function patch(p: Partial<Draft>) {
    // Object.assign onto a fresh instance rather than {...d, ...p}: spreading
    // a generated model class drops its prototype methods, and the result no
    // longer typechecks as a Config.
    setDraft((d) => (d ? Object.assign(new main.Config(), d, p) : d));
  }

  async function save() {
    if (!draft) return;
    // An empty group would organise nothing and confuse the matcher: stop
    // before the backend ever sees it, and say which row is at fault.
    if (draft.organizeEnabled) {
      const bad = draft.organizeGroups.findIndex(
        (g) => !(g.name || "").trim() || (g.exts ?? []).length === 0,
      );
      if (bad >= 0) {
        setTab("下载");
        note(
          `第 ${bad + 1} 个分组还没填完整：文件夹名和扩展名都不能为空，填好或删掉它再保存`,
        );
        return;
      }
    }
    try {
      await api.setConfig(draft as never);
      onSaved(draft);
      note("已保存");
    } catch (e) {
      note(e instanceof Error ? e.message : String(e));
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
      <div className="settings-tabs" role="tablist" aria-label="设置分类">
        {TABS.map((t) => (
          <button
            key={t}
            type="button"
            role="tab"
            aria-selected={tab === t}
            className={`settings-tab${tab === t ? " settings-tab--active" : ""}`}
            onClick={() => setTab(t)}
          >
            {t}
          </button>
        ))}
      </div>

      <section className="settings-section" hidden={tab !== "下载"}>
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

        <h4 className="settings-subsection__title">并行</h4>

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
            <span className="setting__title">最小分片大小</span>
            <span className="setting__hint">
              小于此大小的文件不再拆成多连接，拆了也快不了。单位
              KB；填 0 表示用后端默认（1024 KB）。改完下个任务生效。
            </span>
          </span>
          <input
            type="number"
            min={0}
            max={1048576}
            step={64}
            style={{ width: 110 }}
            value={Math.round((draft.minSegmentSize ?? 0) / 1024)}
            onChange={(e) =>
              patch({
                minSegmentSize: Math.max(0, Number(e.target.value) || 0) * 1024,
              })
            }
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

        <h4 className="settings-subsection__title">限速</h4>

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

        <h4 className="settings-subsection__title">显示</h4>

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">界面刷新速度</span>
            <span className="setting__hint">
              进度数字多久跳一次。只影响显示，不影响真实下载速度；改完直播生效。
            </span>
          </span>
          <select
            style={{ width: 150 }}
            value={draft.refreshRate || "fast"}
            onChange={(e) =>
              patch({ refreshRate: e.target.value as "fast" | "medium" | "slow" })
            }
          >
            <option value="fast">快（250ms）</option>
            <option value="medium">中（650ms）</option>
            <option value="slow">慢（1s）</option>
          </select>
        </label>

                <label className="setting">
          <span className="setting__text">
            <span className="setting__title">按大小</span>
            <span className="setting__hint">
              下了多少才写一次续传记录，单位 KB。越大写盘越少、崩溃重下越多。改完下个任务生效。
            </span>
          </span>
          <span className="field__row" style={{ flex: "0 0 auto" }}>
            <input
              type="number"
              min={64}
              max={65536}
              step={64}
              style={{ width: 110 }}
              value={draft.resumeFlushKb}
              disabled={!draft.resumeFlushBySize}
              onChange={(e) =>
                patch({ resumeFlushKb: Math.max(64, Number(e.target.value) || 1024) })
              }
            />
            <span className="switch">
              <input
                type="checkbox"
                aria-label="启用按大小保存"
                checked={draft.resumeFlushBySize}
                onChange={(e) => {
                  const v = e.target.checked;
                  if (!v && !draft.resumeFlushByTime && !draft.resumeFlushByPct) return;
                  patch({ resumeFlushBySize: v });
                }}
              />
              <span className="switch__track" />
              <span className="switch__thumb" />
            </span>
          </span>
        </label>

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">按时间</span>
            <span className="setting__hint">
              最多隔多久写一次续传记录，单位毫秒。越大写盘越少、崩溃重下越多。改完下个任务生效。
            </span>
          </span>
          <span className="field__row" style={{ flex: "0 0 auto" }}>
            <input
              type="number"
              min={100}
              max={10000}
              step={50}
              style={{ width: 110 }}
              value={draft.resumeFlushMs}
              disabled={!draft.resumeFlushByTime}
              onChange={(e) =>
                patch({ resumeFlushMs: Math.min(10000, Math.max(100, Number(e.target.value) || 250)) })
              }
            />
            <span className="switch">
              <input
                type="checkbox"
                aria-label="启用按时间保存"
                checked={draft.resumeFlushByTime}
                onChange={(e) => {
                  const v = e.target.checked;
                  if (!v && !draft.resumeFlushBySize && !draft.resumeFlushByPct) return;
                  patch({ resumeFlushByTime: v });
                }}
              />
              <span className="switch__track" />
              <span className="switch__thumb" />
            </span>
          </span>
        </label>

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">按百分比</span>
            <span className="setting__hint">
              每下完百分之几写一次。跟上面启用的项谁先到谁写。改完下个任务生效。
            </span>
          </span>
          <span className="field__row" style={{ flex: "0 0 auto" }}>
            <input
              type="number"
              min={0}
              max={100}
              step={1}
              style={{ width: 110 }}
              value={draft.resumeFlushPct}
              disabled={!draft.resumeFlushByPct}
              onChange={(e) =>
                patch({ resumeFlushPct: Math.min(100, Math.max(0, Number(e.target.value) || 0)) })
              }
            />
            <span className="switch">
              <input
                type="checkbox"
                aria-label="启用按百分比保存"
                checked={draft.resumeFlushByPct}
                onChange={(e) => {
                  const v = e.target.checked;
                  if (!v && !draft.resumeFlushBySize && !draft.resumeFlushByTime) return;
                  patch({ resumeFlushByPct: v });
                }}
              />
              <span className="switch__track" />
              <span className="switch__thumb" />
            </span>
          </span>
        </label>

<Switch
          title="每次下载先问保存位置"
          hint="浏览器和剪贴板来的下载会先弹窗确认位置，关掉弹窗等于放弃这次下载。关闭后按上面的自动开始/剪贴板规则直接建任务。"
          checked={draft.askLocation}
          onChange={(v) => patch({ askLocation: v })}
        />

        <p className="settings-section__note">
          开启上面一项时，若浏览器也开了“下载前询问保存位置”，同一个文件会弹两次——请关掉浏览器那边的。
        </p>

        <Switch
          title="按类型自动整理"
          hint="自动到来的下载（非弹窗手动创建的）在完成后按扩展名搬进下面的分组文件夹。未命中任何分组的留在原处。"
          checked={draft.organizeEnabled}
          onChange={(v) => patch({ organizeEnabled: v })}
        />

        {draft.organizeEnabled ? (
          <>
            <label className="setting">
              <span className="setting__text">
                <span className="setting__title">整理根目录</span>
                <span className="setting__hint">
                  {draft.organizeDir || "系统下载文件夹"}（和上面的默认保存目录无关）
                </span>
              </span>
              <span className="field__row" style={{ flex: "0 0 auto" }}>
                <button
                  type="button"
                  className="btn"
                  onClick={() => {
                    void api
                      .pickDirectory()
                      .then((picked) => {
                        if (picked) patch({ organizeDir: picked });
                      })
                      .catch(() => undefined);
                  }}
                >
                  浏览
                </button>
              </span>
            </label>
            {draft.organizeGroups.map((g, i) => (
              <div className="setting" key={i}>
                <span className="setting__text">
                  <span className="field__row" style={{ flex: "1 1 auto" }}>
                    <input
                      type="text"
                      aria-label="文件夹名"
                      style={{ width: 110 }}
                      value={g.name}
                      spellCheck={false}
                      onChange={(e) => {
                        const groups: OrganizeGroup[] =
                          draft.organizeGroups.slice();
                        groups[i] = { ...g, name: e.target.value } as OrganizeGroup;
                        patch({ organizeGroups: groups });
                      }}
                    />
                    <input
                      type="text"
                      aria-label="扩展名，用逗号或空格分隔"
                      style={{ flex: "1 1 auto" }}
                      placeholder="exe, msi"
                      value={g.exts.join(", ")}
                      spellCheck={false}
                      onChange={(e) => {
                        const groups: OrganizeGroup[] =
                          draft.organizeGroups.slice();
                        groups[i] = {
                          ...g,
                          exts: e.target.value
                            .split(/[,，\s]+/)
                            .map((s) => s.trim())
                            .filter((s) => s !== ""),
                        } as OrganizeGroup;
                        patch({ organizeGroups: groups });
                      }}
                    />
                    <button
                      type="button"
                      className="btn"
                      title="直接指定这个分组的文件夹（绝对路径），不跟下载目录走"
                      onClick={() => pickGroupDir(i)}
                    >
                      浏览
                    </button>
                  </span>
                  {groupPlace(draft.organizeDir, g) ? (
                    <span className="setting__hint">
                      文件会进 {groupPlace(draft.organizeDir, g)}
                      {g.dir.trim() ? "" : "（跟整理根目录走，换电脑也有效）"}
                    </span>
                  ) : null}
                </span>
                <button
                  type="button"
                  className="btn"
                  onClick={() => {
                    patch({
                      organizeGroups: draft.organizeGroups.filter(
                        (_, j) => j !== i,
                      ),
                    });
                  }}
                >
                  删除
                </button>
              </div>
            ))}
            <div className="setting">
              <span className="setting__text">
                <span className="setting__hint">
                  文件夹在该组第一个文件完成后才创建；分组按顺序匹配，命中的第一个生效。
                </span>
              </span>
              <span className="field__row" style={{ flex: "0 0 auto" }}>
                <button
                  type="button"
                  className="btn"
                  onClick={() => {
                    patch({
                      organizeGroups: [
                        ...draft.organizeGroups,
                        { name: "", exts: [], dir: "" } as OrganizeGroup,
                      ],
                    });
                  }}
                >
                  添加分组
                </button>
                <button
                  type="button"
                  className="btn"
                  onClick={() => {
                    void api
                      .defaultOrganizeGroups()
                      .then((groups) => patch({ organizeGroups: groups }))
                      .catch(() => undefined);
                  }}
                >
                  恢复默认
                </button>
              </span>
            </div>
          </>
        ) : null}
      </section>



      <section className="settings-section" hidden={tab !== "浏览器扩展"}>
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

        <Switch
          title="验证浏览器身份"
          hint="开着时，每次接管先确认对方是正版签名的浏览器，不是冒充的程序。默认开；每次下载约多花 1 毫秒（首次认出某个浏览器时约 300 毫秒，只一次）。改完立刻生效，不用重启。关了就是秒接，但本机任何程序都能冒充扩展下发任务。"
          checked={draft.verifyPeerProcess ?? true}
          disabled={!draft.extensionEnabled}
          onChange={(v) => patch({ verifyPeerProcess: v })}
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
              max={1048576}
              step={64}
              style={{ width: 110 }}
              value={draft.extensionMinSizeKb}
              onChange={(e) =>
                patch({
                  extensionMinSizeKb: Math.max(
                    0,
                    Math.round(Number(e.target.value) || 0),
                  ),
                })
              }
              disabled={!draft.extensionEnabled}
            />
            <span className="field__suffix">KB</span>
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

      <section className="settings-section" hidden={tab !== "计划下载"}>
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



      <section className="settings-section" hidden={tab !== "完成后"}>
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

      <section className="settings-section" hidden={tab !== "窗口"}>
        <h3 className="settings-section__title">窗口</h3>

        <Switch
          title="关闭窗口时最小化到托盘"
          hint="下载还在进行时不会退出，避免中断传输。"
          checked={draft.minimizeToTray}
          onChange={(v) => patch({ minimizeToTray: v })}
        />
      </section>

      <section className="settings-section" hidden={tab !== "剪贴板"}>
        <h3 className="settings-section__title">剪贴板</h3>

        <Switch
          title="监控剪贴板链接"
          hint="复制一个下载链接后自动识别。关闭时不做任何后台轮询。"
          checked={draft.clipboardWatch}
          onChange={(v) => patch({ clipboardWatch: v })}
        />

        {/* The three outcomes are mutually exclusive, like a radio group: the
            backend resolves them in AskLocation {" > "} Confirm {" > "}
            AutoStart order, so the switches mirror that priority instead of
            letting two contradictory options stay on together. */}
        <Switch
          title="识别到链接时直接开始下载"
          hint="开启后不再询问，复制完链接就开始下；同时会关掉下面的弹窗确认。"
          checked={draft.clipboardAutoStart}
          disabled={!draft.clipboardWatch}
          nested
          onChange={(v) => {
            if (v) {
              patch({ clipboardAutoStart: true, clipboardConfirm: false });
              note("已开启直接下载，弹窗确认已关闭（与后端优先级一致）");
            } else {
              patch({ clipboardAutoStart: false });
            }
          }}
        />

        <Switch
          title="识别到链接时先弹窗确认"
          hint="开启后会先问你要不要下载；同时会关掉上面的直接下载。"
          checked={draft.clipboardConfirm}
          disabled={!draft.clipboardWatch}
          nested
          onChange={(v) => {
            if (v) {
              patch({ clipboardConfirm: true, clipboardAutoStart: false });
              note("已开启弹窗确认，直接下载已关闭（与后端优先级一致）");
            } else {
              patch({ clipboardConfirm: false });
            }
          }}
        />

        <Switch
          title="只加入列表，不自动开始"
          hint="复制链接后任务进入列表并保持排队，由你决定何时开始。"
          checked={
            draft.clipboardWatch &&
            !draft.clipboardAutoStart &&
            !draft.clipboardConfirm
          }
          disabled={!draft.clipboardWatch}
          nested
          onChange={() => {
            patch({ clipboardAutoStart: false, clipboardConfirm: false });
            note("已切换为只加入列表");
          }}
        />

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">检测间隔</span>
            <span className="setting__hint">
              越短响应越快，但会稍微多占用一点 CPU。默认 800 毫秒。需先打开上面的监控才会生效。
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

      <section className="settings-section" hidden={tab !== "软件更新"}>
        <h3 className="settings-section__title">软件更新</h3>

        <div className="setting">
          <span className="setting__text">
            <span className="setting__title">当前版本 v{version || "…"}</span>
            <span className="setting__hint">{updLine()}</span>
          </span>
          <span className="field__row" style={{ flex: "0 0 auto" }}>
            <button
              type="button"
              className="btn"
              disabled={updBusy}
              onClick={checkNow}
            >
              检查更新
            </button>
            {upd && upd.version && upd.state !== "ready" ? (
              <button
                type="button"
                className="btn btn--primary"
                disabled={updBusy || upd.state === "downloading"}
                onClick={startDl}
              >
                {upd.state === "downloading" ? "下载中…" : "立即更新"}
              </button>
            ) : null}
            {upd && upd.state === "ready" ? (
              <button
                type="button"
                className="btn btn--primary"
                disabled={updBusy}
                onClick={applyUpd}
              >
                重启生效
              </button>
            ) : null}
          </span>
        </div>

        <p className="settings-section__note">
          更新会先暂停全部任务再换文件，重启后任务都在，可继续下载。
          下载中的新版文件会做哈希校验，失败就丢弃重来。
        </p>
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
