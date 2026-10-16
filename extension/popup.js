/**
 * The popup: is the app running, and a switch for the interception.
 *
 * The switch is a shortcut, not a second copy of the setting — both it and the
 * app's settings page write to the same place, so they cannot disagree.
 */

const dot = document.getElementById("dot");
const state = document.getElementById("state");
const toggle = document.getElementById("toggle");
const hint = document.getElementById("hint");
const ver = document.getElementById("ver");

/**
 * The last minSizeKb the app reported, so the optimistic repaint after a
 * toggle keeps showing the real limit instead of a hard-coded 0 ("take
 * everything") until the fresh status answer arrives.
 */
let lastMinSizeKb = 0;

/** Paints one state, so the places that report it agree. */
function render(status, enabled) {
  const running = Boolean(status && status.running);
  dot.className = `dot ${running ? "dot--on" : "dot--off"}`;
  state.textContent = running ? "VKMDownload 正在运行" : "VKMDownload 没有运行";

  toggle.disabled = !running;
  toggle.textContent = enabled ? "暂停接管下载" : "恢复接管下载";

  if (typeof status.minSizeKb === "number") lastMinSizeKb = status.minSizeKb;

  if (!running) {
    hint.textContent = "启动 VKMDownload 后，下载会交给它。";
  } else if (!enabled) {
    hint.textContent = "接管已暂停，下载仍由浏览器完成。";
  } else if (status.minSizeKb > 0) {
    const mb = (status.minSizeKb / 1024).toFixed(status.minSizeKb % 1024 ? 1 : 0);
    hint.textContent = `小于 ${mb} MB 的文件交给浏览器，其余由 VKMDownload 下载。想改的话在应用的「设置 → 浏览器扩展」里。`;
  } else {
    hint.textContent = "所有下载都会交给 VKMDownload。可在应用设置里调整。";
  }
}

/** A refusal from the app's peer check carries its reason verbatim. */
function showRefused(result) {
  if (result && result.refused) {
    hint.textContent = result.error || "来电不是受信浏览器";
    return true;
  }
  return false;
}

async function refresh() {
  try {
    const status = await chrome.runtime.sendMessage("status");
    if (!status) throw new Error("no response");
    if (showRefused(status)) return;
    render(status, status.enabled);
  } catch {
    dot.className = "dot dot--off";
    state.textContent = "无法联系扩展后台";
    toggle.disabled = true;
  }
  try {
    ver.textContent = `扩展 v${chrome.runtime.getManifest().version}`;
  } catch {
    // Unbranded contexts (tests): leave the version line blank.
  }
}

toggle.addEventListener("click", async () => {
  toggle.disabled = true;
  const result = await chrome.runtime.sendMessage("toggle");
  if (!result) {
    // The app went away between the status check and the click.
    void refresh();
    return;
  }
  if (showRefused(result)) {
    toggle.disabled = false;
    return;
  }
  render({ running: true, minSizeKb: lastMinSizeKb }, result.enabled);
  void refresh();
});

void refresh();
