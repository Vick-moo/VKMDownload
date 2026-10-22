/**
 * The popup: is the app running, is interception on, and is this extension old
 * enough to want updating.
 *
 * It is a status display, not a control surface. The switches live in the app's
 * settings and only the app writes them (the endpoint answers 405 to
 * POST /settings), so the popup shows the current mode and points at the place
 * that changes it. The one action left is 重新配对, which only forgets a key.
 */

const dot = document.getElementById("dot");
const state = document.getElementById("state");
const mode = document.getElementById("mode");
const repair = document.getElementById("repair");
const hint = document.getElementById("hint");
const ver = document.getElementById("ver");
const verwarn = document.getElementById("verwarn");

/** The extension's own version, or "" outside a real extension context. */
function extensionVersion() {
  try {
    return String(chrome.runtime.getManifest().version || "");
  } catch {
    return "";
  }
}

/** Paints one state, so the places that report it agree. */
function render(status) {
  const running = Boolean(status && status.running);
  dot.className = `dot ${running ? "dot--on" : "dot--off"}`;
  state.textContent = running ? "VKMDownload 正在运行" : "VKMDownload 没有运行";

  mode.textContent = !running
    ? "接管状态未知"
    : status.enabled
      ? "接管已开启"
      : "接管已暂停";

  // The app is unreachable. An existing pairing is never replaced silently —
  // doing so would let anything that grabs the port hand the extension its own
  // key — so a reinstall or a deleted pair.key looks exactly like "not running".
  // The way out therefore has to be visible.
  if (repair) repair.hidden = running;

  if (!running) {
    hint.textContent = "启动 VKMDownload 后，下载会交给它。若已启动仍显示未运行，点「重新配对」。";
  } else if (!status.enabled) {
    hint.textContent = "接管已暂停，下载仍由浏览器完成。";
  } else if (status.minSizeKb > 0) {
    const mb = (status.minSizeKb / 1024).toFixed(status.minSizeKb % 1024 ? 1 : 0);
    hint.textContent = `小于 ${mb} MB 的文件交给浏览器，其余由 VKMDownload 下载。`;
  } else {
    hint.textContent = "所有下载都会交给 VKMDownload。";
  }
}

/** Shows this extension's version and the app's minimum, with a warning. */
function renderVersion(status) {
  const mine = extensionVersion() || (status && status.extVersion) || "";
  const min =
    status && typeof status.minExtensionVersion === "string"
      ? status.minExtensionVersion
      : "";

  if (ver) {
    ver.textContent = !mine
      ? "扩展版本未知"
      : min
        ? `扩展 v${mine} · 应用要求 ≥ ${min}`
        : `扩展 v${mine}`;
  }
  if (verwarn) {
    // The app decides: it has both versions. An extension old enough not to
    // receive the field simply shows no warning.
    verwarn.hidden = !(status && status.versionMismatch);
  }
}

async function refresh() {
  try {
    const status = await chrome.runtime.sendMessage("status");
    if (!status) throw new Error("no response");
    render(status);
    renderVersion(status);
  } catch {
    dot.className = "dot dot--off";
    state.textContent = "无法联系扩展后台";
    mode.textContent = "接管状态未知";
    if (verwarn) verwarn.hidden = true;
    renderVersion(null);
  }
}

if (repair) {
  repair.addEventListener("click", async () => {
    repair.disabled = true;
    try {
      await chrome.runtime.sendMessage("forgetPairing");
      hint.textContent = "已清除配对，正在重新连接…";
    } catch (error) {
      hint.textContent = `清除配对失败：${shortError(error)}`;
    } finally {
      repair.disabled = false;
      void refresh();
    }
  });
}

/** A one-line, bounded description of an error for the hint line. */
function shortError(error) {
  const text = String((error && error.message) || error || "未知错误");
  return text.length > 80 ? `${text.slice(0, 80)}…` : text;
}

void refresh();
