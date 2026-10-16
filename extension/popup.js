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
const tokenInput = document.getElementById("token");
const saveToken = document.getElementById("saveToken");
const pairStatus = document.getElementById("pairStatus");
const ver = document.getElementById("ver");

/** Shows whether a pairing token is stored: the feature's on/off signal. */
function paintPair(paired) {
  pairStatus.textContent = paired ? "已配对" : "未配对（免配对模式）";
  pairStatus.className = `pair-status${paired ? "" : " pair-status--off"}`;
}

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

/** The pairing failure text the background forwards (see PAIR_HINT there). */
function showPairError(result) {
  if (result && result.pairError) {
    hint.textContent = result.error || "配对失败：去 App 设置→浏览器配对复制 token";
    return true;
  }
  return false;
}

async function refresh() {
  try {
    const status = await chrome.runtime.sendMessage("status");
    if (!status) throw new Error("no response");
    if (showPairError(status)) return;
    render(status, status.enabled);
  } catch {
    dot.className = "dot dot--off";
    state.textContent = "无法联系扩展后台";
    toggle.disabled = true;
  }
  try {
    const got = await chrome.storage.local.get("apiToken");
    const paired = Boolean(got && typeof got.apiToken === "string" && got.apiToken);
    if (paired) tokenInput.value = got.apiToken;
    paintPair(paired);
  } catch {
    // Storage unavailable: the pairing box simply stays empty.
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
  if (showPairError(result)) {
    toggle.disabled = false;
    return;
  }
  render({ running: true, minSizeKb: lastMinSizeKb }, result.enabled);
  void refresh();
});

saveToken.addEventListener("click", async () => {
  const token = (tokenInput.value || "").trim();
  try {
    if (token) {
      await chrome.storage.local.set({ apiToken: token });
    } else {
      await chrome.storage.local.remove("apiToken");
    }
    hint.textContent = token ? "配对 token 已保存。" : "已清除配对 token（恢复免配对模式）。";
    paintPair(Boolean(token));
  } catch {
    hint.textContent = "保存失败：扩展存储不可用。";
  }
});

void refresh();
