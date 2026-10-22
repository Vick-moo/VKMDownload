import { useEffect, useRef, useState } from "react";
import type { JSX } from "react";
import { main } from "../../wailsjs/go/models";
import {
  DeleteUpdateBackup,
  ExtensionStatus,
  PairingInfo,
  PeerDiagnostics,
  ResetPairing,
  UpdateBackupInfo,
} from "../../wailsjs/go/main/App";
import { api } from "../api";
import { formatBytes } from "../format";
import type { Config, OrganizeGroup, StoredSecretSummary, UpdateStatus } from "../types";
import { ConfirmDialog } from "./ConfirmDialog";
import { IconFolder } from "./Icons";
import { formatLastSeen, PeerDiagnosticsList } from "./PeerDiagnosticsList";
import { UpdateNotesDialog } from "./UpdateNotesDialog";

interface Props {
  config: Config | null;
  /** From the binding; shown at the foot of the page. */
  version: string;
  /** One-sentence report on the last update attempt, from the parent. */
  updateNotice?: string;
  onSaved: (cfg: Config) => void;
}

/**
 * The editable shape. `verifyPeerProcess` is declared optional so the switch
 * survives until save even against a generated TS model that predates the
 * Go Config field; current models already include it, making this harmless.
 */
type Draft = Config & { verifyPeerProcess?: boolean };

/**
 * The Config made safe for the editor.
 *
 * A nil slice on the Go side arrives as null, and this page indexes straight
 * into organizeGroups (map/filter/slice) whenever organise is enabled, so a
 * null there white-screens the window. Normalising once here keeps every use
 * below free of `?? []`. Object.assign onto a fresh candidate matches patch()
 * and preserves the generated model's prototype.
 */
function normalizeConfig(config: Config | null): Draft | null {
  if (!config) return null;
  const draft = Object.assign(new main.Config(), config, {
    organizeGroups: config.organizeGroups ?? [],
  });
  // The percent trigger does nothing at 0: manager.go hands it straight to the
  // engine, which reads 0 as "off". A stored switch-on/0 pair therefore showed
  // an enabled control that could never fire, so the editor opens it at the
  // smallest useful value instead of leaving the invalid pair in place.
  if (draft.resumeFlushByPct && draft.resumeFlushPct < 1) {
    draft.resumeFlushPct = 10;
  }
  return draft;
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
 * A small custom radio presented as one option in a compact choice row.
 *
 * The global input styling is aimed at text fields, so this draws the dot with
 * inline styles instead of inheriting that box. The input stays a real radio,
 * so keyboard and screen-reader semantics are unchanged.
 */
function RadioOption({
  name,
  label,
  checked,
  onChange,
}: {
  name: string;
  label: string;
  checked: boolean;
  onChange: () => void;
}): JSX.Element {
  return (
    <label
      style={{
        display: "flex",
        alignItems: "center",
        gap: 6,
        cursor: "pointer",
        fontSize: 13,
        whiteSpace: "nowrap",
      }}
    >
      <input
        type="radio"
        name={name}
        checked={checked}
        onChange={onChange}
        style={{
          appearance: "none",
          width: 14,
          height: 14,
          padding: 0,
          margin: 0,
          flex: "0 0 auto",
          borderRadius: "50%",
          border: `1px solid ${checked ? "var(--accent)" : "var(--border-strong)"}`,
          background: checked ? "var(--accent)" : "transparent",
          // A background-coloured inner ring leaves the accent dot in the
          // middle, so the choice reads as a radio rather than a tiny button.
          boxShadow: checked
            ? "inset 0 0 0 3px var(--bg-surface)"
            : "none",
          cursor: "pointer",
        }}
      />
      {label}
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
  updateNotice,
  onSaved,
}: Props): JSX.Element {
  const [draft, setDraft] = useState<Draft | null>(() => normalizeConfig(config));
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
  // The restore point checked against the file on disk, not just the polled
  // status: it can be deleted or replaced without a status change.
  const [backup, setBackup] = useState<main.UpdateBackup | null>(null);
  // Pairing, extension state and peer refusals are process state, not Config.
  const [pairing, setPairing] = useState<main.PairingStatus | null>(null);
  const [extStatus, setExtStatus] = useState<main.ExtensionInfo | null>(null);
  const [diagnostics, setDiagnostics] = useState<main.PeerRejection[]>([]);
  const [resetting, setResetting] = useState(false);
  const [confirmResetOpen, setConfirmResetOpen] = useState(false);
  // What the app keeps on disk, read when the 隐私 tab is opened, and the busy
  // flag for the one action that removes it.
  const [stored, setStored] = useState<StoredSecretSummary | null>(null);
  const [clearing, setClearing] = useState(false);
  // Clearing history is the only irreversible action on this page, so it goes
  // through the same confirmation dialog as the bulk row deletions.
  const [confirmClearOpen, setConfirmClearOpen] = useState(false);

  async function refreshStored() {
    try {
      setStored(await api.storedSecrets());
    } catch {
      // Leaving the previous numbers is better than showing a fabricated zero.
    }
  }

  /** Re-reads the on-disk backup record; UpdateBackupInfo checks the file. */
  async function refreshBackup() {
    try {
      setBackup(await UpdateBackupInfo());
    } catch {
      // Distinguish "could not read" from the loading placeholder so the row
      // does not spin forever; the buttons stay disabled either way.
      setBackup({ available: false, version: "", size: 0, path: "" });
    }
  }

  /** Refreshes pairing, extension status and refusals independently. */
  async function refreshSecurity() {
    // One panel failing must not blank the other two; the Refresh button and
    // the tab-open effect both retry.
    await Promise.all([
      PairingInfo()
        .then(setPairing)
        .catch(() => {}),
      ExtensionStatus()
        .then(setExtStatus)
        .catch(() => {}),
      PeerDiagnostics()
        .then(setDiagnostics)
        .catch(() => {}),
    ]);
  }

  /** The data directory behind PairingStatus.KeyPath (<dir>\pair.key). */
  function dataDirOf(keyPath: string): string {
    const cut = Math.max(keyPath.lastIndexOf("\\"), keyPath.lastIndexOf("/"));
    return cut > 0 ? keyPath.slice(0, cut) : keyPath;
  }

  /** One sentence describing what is on disk, in counts only. */
  function storedLine(): string {
    if (!stored) return "正在读取…";
    const parts = [`${stored.taskCount} 条下载记录`];
    if (stored.withCookie > 0) parts.push(`${stored.withCookie} 条带登录信息（已加密）`);
    if (stored.withQueryUrl > 0) parts.push(`${stored.withQueryUrl} 条链接带签名参数`);
    const log = stored.logBytes > 0 ? `${(stored.logBytes / 1024).toFixed(0)} KB 日志` : "没有日志";
    return `${parts.join("、")}，${log}。`;
  }

  async function clearHistory() {
    setClearing(true);
    try {
      await api.clearDownloadHistory();
      await refreshStored();
      // The footer status line is visible on the privacy tab; updMsg belongs
      // to the update section and would have hidden this result one tab away.
      note("已清除下载记录与日志。已下载的文件仍然保留。");
    } catch (e) {
      note(`清除失败：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setClearing(false);
      // The dialog stays up, disabled, while the request runs so a second
      // click cannot start another one; close it when the result is in.
      setConfirmClearOpen(false);
    }
  }
  // The version whose "what's new" dialog is open; the actual download starts
  // from the dialog's confirm, not straight from the button.
  const [notesVer, setNotesVer] = useState<string | null>(null);
  const pollTimer = useRef<number | null>(null);


  useEffect(() => {
    setDraft(normalizeConfig(config));
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

  // The backup record lives on disk, not in the polled status, so read it when
  // the update tab opens and again whenever the update state moves (a failed
  // apply, a finished download, a post-restart hydration).
  useEffect(() => {
    if (tab !== "软件更新") return;
    let cancelled = false;
    void UpdateBackupInfo()
      .then((b) => {
        if (!cancelled) setBackup(b);
      })
      .catch(() => {
        if (!cancelled) {
          setBackup({ available: false, version: "", size: 0, path: "" });
        }
      });
    return () => {
      cancelled = true;
    };
  }, [tab, upd?.state, upd?.version]);

  // Pairing and diagnostics are process state, not Config: read them whenever
  // the extension tab is opened (including returning to it).
  useEffect(() => {
    if (tab !== "浏览器扩展") return;
    void refreshSecurity();
  }, [tab]);

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
        setUpd((prev) => ({
          state: "idle",
          version: info.version,
          downloaded: 0,
          total: 0,
          error: "",
          // Finding a new release does not erase what the previous staging
          // attempt verified; the panel keeps showing the last verdict.
          digestPresent: prev?.digestPresent,
          sizeMatched: prev?.sizeMatched,
          verified: prev?.verified,
          backupAvailable: prev?.backupAvailable,
          backupVersion: prev?.backupVersion,
          backupSize: prev?.backupSize,
        }));
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
      // If the swap actually started the process is gone; if it did not, the
      // backup may have been created or consumed and the row must reflect it.
      void refreshBackup();
    }
  }

  // Manual restore for an update that is running but was never confirmed.
  // The backend hands off to the same hidden helper an update uses: this
  // process quits, the helper puts the backup back in place and starts it.
  // Reaching the line after the call means the quit was refused again.
  async function restoreOldVersion() {
    setUpdBusy(true);
    setUpdMsg("");
    try {
      await api.rollbackUpdate();
      setUpdMsg("还原未开始：程序没有退出。");
    } catch (e) {
      setUpdMsg(`还原失败：${e instanceof Error ? e.message : String(e)}`);
      // A stale backup record is the usual reason this fails; re-read it so
      // the row stops offering a restore that cannot work.
      await refreshBackup();
    } finally {
      setUpdBusy(false);
    }
  }

  /** Removes the restore point, then re-reads the record so the row settles. */
  async function deleteBackup() {
    setUpdBusy(true);
    setUpdMsg("");
    try {
      await DeleteUpdateBackup();
      await refreshBackup();
      setUpdMsg("已删除旧版本备份。");
    } catch (e) {
      setUpdMsg(
        `删除旧版本备份失败：${e instanceof Error ? e.message : String(e)}`,
      );
      await refreshBackup();
    } finally {
      setUpdBusy(false);
    }
  }

  /** Deletes the global pairing key and re-reads the security panels. */
  async function resetPairing() {
    setResetting(true);
    try {
      await ResetPairing();
      await refreshSecurity();
      note("已重置配对，所有浏览器都需要重新配对。");
    } catch (e) {
      note(`重置配对失败：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setResetting(false);
      setConfirmResetOpen(false);
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
    // "current" is the normal answer when there is nothing to install. It is a
    // separate state rather than "failed" so the row does not paint an error
    // and offer a retry for the expected outcome of pressing the button.
    if (upd.state === "current") return `已是最新版本（${upd.version}）。`;
    if (upd.state === "failed" && upd.error) return `更新失败：${upd.error}`;
    return "自动检查新版本，有更新时顶部会出现横幅。";
  }

  /** What the last staged update actually verified; absent means false. */
  function verifyLine(): string {
    const digest = upd?.digestPresent ?? false;
    const size = upd?.sizeMatched ?? false;
    const verified = upd?.verified ?? false;
    return `哈希校验：${digest ? "有" : "无"}；大小一致：${size ? "是" : "否"}；校验通过：${verified ? "是" : "否"}`;
  }

  /** The backup as the on-disk check sees it, not just the polled hint. */
  function backupLine(): string {
    if (!backup) return "正在读取旧版本备份…";
    if (!backup.available) return "没有可还原的旧版本备份。";
    return `旧版本 ${backup.version || "未知"}（${formatBytes(backup.size)}）`;
  }

  if (!draft) {
    return (
      <div className="empty">
        <p className="empty__hint">正在读取设置…</p>
      </div>
    );
  }

  const TABS = ["下载", "浏览器扩展", "计划下载", "剪贴板", "完成后", "窗口", "软件更新", "隐私"];

  // Read the on-disk summary when the tab is opened, so the numbers are not a
  // stale snapshot from whenever the page loaded.
  useEffect(() => {
    if (tab === "隐私") void refreshStored();
  }, [tab]);

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
              每下完百分之几写一次，开启后至少 1%；留 0 会按 10% 处理（不会关闭触发器）。跟上面启用的项谁先到谁写。改完下个任务生效。
            </span>
          </span>
          <span className="field__row" style={{ flex: "0 0 auto" }}>
            <input
              type="number"
              min={1}
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
                  // Turning it on while the value is 0 would leave a switch
                  // that saves nothing; move it to a default that works.
                  if (v && draft.resumeFlushPct < 1) {
                    patch({ resumeFlushByPct: true, resumeFlushPct: 10 });
                    return;
                  }
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

        <h4 className="settings-subsection__title">配对</h4>

        <div className="setting">
          <span className="setting__text">
            <span className="setting__title">
              配对状态：
              {pairing ? (pairing.paired ? "已配对" : "未配对") : "正在读取…"}
            </span>
            <span className="setting__hint">
              指纹：
              <code style={{ fontFamily: "var(--font-mono)" }}>
                {pairing?.fingerprint || "—"}
              </code>
              {" · "}端口：{pairing?.port ?? "—"}
            </span>
            <span className="setting__hint" style={{ wordBreak: "break-all" }}>
              数据目录：
              <code style={{ fontFamily: "var(--font-mono)" }}>
                {pairing ? dataDirOf(pairing.keyPath) : "—"}
              </code>
            </span>
          </span>
          <button
            type="button"
            className="btn"
            disabled={resetting}
            onClick={() => setConfirmResetOpen(true)}
          >
            重置配对
          </button>
        </div>

        <p className="settings-section__note">
          钥匙是全局一份，不能只断开某一个浏览器。
        </p>

        <h4 className="settings-subsection__title">Origin 锁定</h4>

        <div className="setting setting--stacked">
          <span className="setting__text">
            <span className="setting__title">当前扩展 Origin</span>
            <span className="setting__hint">
              <code
                style={{
                  fontFamily: "var(--font-mono)",
                  wordBreak: "break-all",
                }}
              >
                {extStatus?.lastOrigin || "尚未检测到扩展连接"}
              </code>
            </span>
          </span>
          <span className="field__row" style={{ flexWrap: "wrap" }}>
            <input
              type="text"
              readOnly
              value={draft.extensionOriginLock}
              placeholder="未锁定（任何已装扩展都能请求）"
              aria-label="已锁定的扩展 Origin"
              style={{ minWidth: 320, fontFamily: "var(--font-mono)" }}
            />
            <button
              type="button"
              className="btn"
              disabled={
                !extStatus?.lastOrigin ||
                extStatus.lastOrigin === draft.extensionOriginLock
              }
              onClick={() => {
                if (!extStatus?.lastOrigin) return;
                patch({ extensionOriginLock: extStatus.lastOrigin });
                note("已填入当前扩展 Origin；点右下角「保存设置」后生效。");
              }}
            >
              锁定当前扩展
            </button>
            <button
              type="button"
              className="btn"
              disabled={!draft.extensionOriginLock}
              onClick={() => {
                patch({ extensionOriginLock: "" });
                note("已清空 Origin 锁定；保存后任何已装扩展都能请求。");
              }}
            >
              解除锁定
            </button>
          </span>
          <span className="setting__hint">
            锁定后只有这个扩展能连；清空则任何已装扩展都能请求（当前默认）。不要加尾斜杠，必须与浏览器发送的 Origin 完全一致。
          </span>
        </div>

        <h4 className="settings-subsection__title">扩展状态</h4>

        <div className="setting">
          <span className="setting__text">
            <span className="setting__title">最近连接</span>
            {extStatus ? (
              <>
                <span className="setting__hint">
                  {formatLastSeen(extStatus.lastSeen)}
                  {" · "}扩展版本：{extStatus.extVersion || "未知"}
                  {"（"}
                  {extStatus.extVersion
                    ? extStatus.versionMismatch
                      ? "版本不匹配"
                      : "版本匹配"
                    : "无版本信息"}
                  {"） · "}接受 {extStatus.accepted} / 拒绝{" "}
                  {extStatus.rejected}
                </span>
                {extStatus.versionMismatch ? (
                  <span
                    className="setting__hint"
                    style={{ color: "var(--warning)" }}
                  >
                    扩展版本过旧，请去 chrome://extensions 重新加载/更新
                  </span>
                ) : null}
              </>
            ) : (
              <span className="setting__hint">正在读取扩展状态…</span>
            )}
          </span>
          <button
            type="button"
            className="btn"
            onClick={() => void refreshSecurity()}
          >
            刷新
          </button>
        </div>

        <h4 className="settings-subsection__title">被拒绝的连接</h4>
        <PeerDiagnosticsList items={diagnostics} />

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
          hint="计划完成后自动关机；关机前会等下面的延迟，期间运行 shutdown /a 可以取消。"
          checked={draft.planShutdown}
          onChange={(v) => patch({ planShutdown: v })}
        />

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">关机延迟</span>
            <span className="setting__hint">
              0 = 计划完成后立即关机；默认 60 秒，最长 600 秒。
            </span>
          </span>
          <span className="field__row" style={{ flex: "0 0 auto" }}>
            <input
              type="number"
              min={0}
              max={600}
              step={10}
              style={{ width: 96 }}
              value={draft.shutdownDelaySeconds}
              disabled={!draft.planShutdown}
              onChange={(e) => {
                const raw = e.target.value;
                if (raw === "") {
                  // 0 is a meaningful value here (shut down immediately), so
                  // clearing the box must not silently mean that.
                  patch({ shutdownDelaySeconds: 60 });
                  return;
                }
                const n = Number(raw);
                patch({
                  shutdownDelaySeconds: Number.isFinite(n)
                    ? Math.min(600, Math.max(0, Math.round(n)))
                    : 60,
                });
              }}
            />
            <span className="field__suffix">秒</span>
          </span>
        </label>
      </section>



      <section className="settings-section" hidden={tab !== "完成后"}>
        <h3 className="settings-section__title">完成后</h3>

        <div className="setting">
          <span className="setting__text">
            <span className="setting__title">下载完成后</span>
            <span className="setting__hint">
              三选一；后端保证「打开文件」和「打开目录」互斥，同时为真时保留目录。
            </span>
          </span>
          <span
            className="field__row"
            style={{ flex: "0 0 auto", flexWrap: "wrap", justifyContent: "flex-end" }}
          >
            <RadioOption
              name="completionAction"
              label="打开文件"
              checked={draft.openFileWhenDone && !draft.openDirWhenDone}
              onChange={() =>
                patch({ openFileWhenDone: true, openDirWhenDone: false })
              }
            />
            <RadioOption
              name="completionAction"
              label="打开目录"
              checked={draft.openDirWhenDone}
              onChange={() =>
                patch({ openFileWhenDone: false, openDirWhenDone: true })
              }
            />
            <RadioOption
              name="completionAction"
              label="都不做"
              checked={!draft.openFileWhenDone && !draft.openDirWhenDone}
              onChange={() =>
                patch({ openFileWhenDone: false, openDirWhenDone: false })
              }
            />
          </span>
        </div>
      </section>

      <section className="settings-section" hidden={tab !== "窗口"}>
        <h3 className="settings-section__title">窗口</h3>

        <Switch
          title="开机自启"
          hint="电脑启动时自动打开；移动程序位置后需要重新勾选。"
          checked={draft.launchAtLogin}
          onChange={(v) => patch({ launchAtLogin: v })}
        />

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
              越短响应越快，但会稍微多占用一点 CPU。默认 800 毫秒，允许 200–10000，低于 200 按 200 处理，清空或 0 按默认 800。需先打开上面的监控才会生效。
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

      <section className="settings-section" hidden={tab !== "隐私"}>
        <h3 className="settings-section__title">本机数据</h3>

        <label className="setting">
          <span className="setting__text">
            <span className="setting__title">历史保留</span>
            <span className="setting__hint">
              已完成、已取消和失败的记录保留多久；永久保留会让记录一直留在本机。
            </span>
          </span>
          <select
            style={{ width: 150 }}
            value={draft.historyRetentionDays}
            onChange={(e) =>
              patch({ historyRetentionDays: Number(e.target.value) })
            }
          >
            {![0, 7, 30, 90].includes(draft.historyRetentionDays) ? (
              <option value={draft.historyRetentionDays}>
                {draft.historyRetentionDays} 天
              </option>
            ) : null}
            <option value={7}>7 天</option>
            <option value={30}>30 天</option>
            <option value={90}>90 天</option>
            <option value={0}>永久（0）</option>
          </select>
        </label>

        <Switch
          title="写诊断日志"
          hint="关闭后不再写日志（更新程序本身的日志除外）。"
          checked={draft.loggingEnabled}
          onChange={(v) => patch({ loggingEnabled: v })}
        />

        <div className="setting">
          <span className="setting__text">
            <span className="setting__title">程序在这台电脑上保留了什么</span>
            <span className="setting__hint">{storedLine()}</span>
          </span>
        </div>

        <div className="setting">
          <span className="setting__text">
            <span className="setting__title">清除下载记录与日志</span>
            <span className="setting__hint">
              删掉任务列表与日志（含旧版本留下的副本）。<strong>已下载的文件、你的设置、配对密钥不会被删。</strong>
            </span>
          </span>
          <span className="field__row" style={{ flex: "0 0 auto" }}>
            <button
              type="button"
              className="btn"
              disabled={clearing}
              onClick={() => setConfirmClearOpen(true)}
            >
              {clearing ? "清除中…" : "清除"}
            </button>
          </span>
        </div>

        <p className="settings-section__note">
          未完成任务的登录信息用 Windows 自带的加密保存，拷到别的电脑解不开；
          但以你的身份运行的恶意程序仍能解密。详见仓库里的 PRIVACY.md。
        </p>
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
                onClick={() => setNotesVer(upd.version)}
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

        <div className="setting">
          <span className="setting__text">
            <span className="setting__title">上次更新校验</span>
            <span className="setting__hint">{verifyLine()}</span>
          </span>
        </div>

        <div className="setting">
          <span className="setting__text">
            <span className="setting__title">旧版本备份</span>
            <span className="setting__hint">{backupLine()}</span>
          </span>
          <span className="field__row" style={{ flex: "0 0 auto" }}>
            <button
              type="button"
              className="btn"
              disabled={updBusy || !backup?.available}
              title={
                backup?.available
                  ? `旧版本 ${backup.version || "未知"}`
                  : updateNotice || "没有可还原的旧版本备份"
              }
              onClick={() => void restoreOldVersion()}
            >
              还原旧版本
            </button>
            <button
              type="button"
              className="btn"
              disabled={updBusy || !backup?.available}
              onClick={() => void deleteBackup()}
            >
              删除旧版本备份
            </button>
          </span>
        </div>

        <Switch
          title="启动时自动检查更新"
          hint="关闭后不再自动联网检查；上面的「检查更新」按钮随时可用。这是本程序除下载外唯一的联网请求。"
          checked={draft.checkUpdatesOnStart}
          onChange={(v) => patch({ checkUpdatesOnStart: v })}
        />

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

      {confirmResetOpen ? (
        <ConfirmDialog
          title="重置配对？"
          message="重置后所有浏览器都需要重新配对。"
          detail="钥匙是全局一份，不能只断开某一个浏览器。"
          confirmLabel="重置配对"
          busy={resetting}
          onConfirm={() => void resetPairing()}
          onCancel={() => setConfirmResetOpen(false)}
        />
      ) : null}

      {confirmClearOpen ? (
        <ConfirmDialog
          title="清除下载记录与日志？"
          message="会删除任务列表、日志，以及旧版本留下的副本。"
          detail="已下载的文件、你的设置、配对密钥不会被删。此操作不可撤销。"
          confirmLabel="清除"
          busy={clearing}
          onConfirm={() => void clearHistory()}
          onCancel={() => setConfirmClearOpen(false)}
        />
      ) : null}

      {notesVer ? (
        <UpdateNotesDialog
          version={notesVer}
          current={version}
          onClose={() => setNotesVer(null)}
          onUpdate={() => {
            setNotesVer(null);
            void startDl();
          }}
        />
      ) : null}

    </div>
  );
}
