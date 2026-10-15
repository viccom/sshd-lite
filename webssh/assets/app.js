"use strict";
// Frontend ported from mole's proven admin UI: WebSSHPage.tsx (terminal UX,
// frame protocol) and FilePanel.tsx (floating SFTP panel with preview).
// Same wire protocol: [type 1B][payload], raw bytes for data frames and
// UTF-8 JSON for file operations.
const MSG = { DATA: 0x01, RESIZE: 0x02, KEEP: 0x03,
  LIST_REQ: 0x04, UPLOAD_REQ: 0x05, UPLOAD_DATA: 0x06, UPLOAD_END: 0x07,
  DOWNLOAD_REQ: 0x08, DELETE_REQ: 0x09, MKDIR_REQ: 0x0A, READ_REQ: 0x0B,
  LIST_RESP: 0x0C, DATA_RESP: 0x0D, ACK_RESP: 0x0E, READ_RESP: 0x0F };

const qs = new URLSearchParams(location.search);
let token = qs.get("token") || localStorage.getItem("webssh_token") || "";
if (!token) {
  token = prompt("webssh token:") || "";
  if (token) localStorage.setItem("webssh_token", token);
  else {
    // cancelled: fail visibly instead of re-prompting in a loop
    document.getElementById("dot").className = "dot offline";
    document.getElementById("status").textContent = "缺少 token（用 ?token= 访问）";
    throw new Error("webssh: no token");
  }
}

const statusEl = document.getElementById("status");
const dotEl = document.getElementById("dot");
document.getElementById("title").textContent = location.host;

// Connection state machine, mirroring mole's WebSSHPage: dot color + text on
// the left, action buttons on the right only while connected, 重新连接 while
// disconnected. The file panel unmounts (hides) on disconnect like mole's
// `showFilePanel && status === 'connected'` guard.
function setState(s, msg) {
  dotEl.className = "dot " + (s === "online" ? "online" : s === "connecting" ? "connecting" : "offline");
  statusEl.textContent = s === "connecting" ? "连接中..." :
    s === "online" ? "已连接" :
    s === "disconnected" ? "已断开" : (msg || "错误");
  const show = (id, on) => { document.getElementById(id).style.display = on ? "" : "none"; };
  show("toggle-files", s === "online");
  show("copy", s === "online");
  show("paste", s === "online");
  show("reconnect", s !== "online" && s !== "connecting");
  if (s !== "online") closePanel();
}

const term = new Terminal({
  cursorBlink: true,
  fontSize: 14,
  fontFamily: 'Menlo, Monaco, Consolas, "Courier New", monospace',
  theme: { background: "#1e1e1e", foreground: "#d4d4d4", cursor: "#d4d4d4",
    selectionBackground: "#264f78" },
});
const fit = new FitAddon.FitAddon();
term.loadAddon(fit);
term.open(document.getElementById("term"));
fit.fit();

// Terminal output needs a STREAMING decoder: SSH chunks can split a
// multi-byte UTF-8 char across two WS frames, and decoding each frame in
// isolation turns the halves into U+FFFD replacement chars. JSON frames are
// whole messages and use a separate decoder.
const dataDec = new TextDecoder();
const dec = new TextDecoder();

let fileHandler = null; // registered by the file panel while it is open

const ws = new WebSocket(
  (location.protocol === "https:" ? "wss://" : "ws://") + location.host +
  "/webssh?token=" + encodeURIComponent(token));
ws.binaryType = "arraybuffer";

function frameRaw(t, bytes) {
  const buf = new Uint8Array(1 + bytes.length);
  buf[0] = t; buf.set(bytes, 1);
  return buf;
}
function sendRaw(t, bytes) { if (ws.readyState === 1) ws.send(frameRaw(t, bytes)); }
function sendJSON(t, obj) { sendRaw(t, new TextEncoder().encode(JSON.stringify(obj))); }

function sendResize() { sendJSON(MSG.RESIZE, { cols: term.cols, rows: term.rows }); }

ws.onopen = () => {
  setState("online");
  sendResize();
  setInterval(() => sendRaw(MSG.KEEP, new Uint8Array(0)), 30000);
  term.focus();
};
ws.onclose = (e) => {
  setState("disconnected");
  if (e.code !== 1000) term.write("\r\n\x1b[31m连接已断开\x1b[0m");
};
ws.onerror = () => setState("error", "WebSocket 连接错误");

ws.onmessage = (ev) => {
  const b = new Uint8Array(ev.data);
  if (b.length === 0) return;
  const t = b[0], payload = b.subarray(1);
  switch (t) {
    case MSG.DATA: term.write(dataDec.decode(payload, { stream: true })); break;
    case MSG.KEEP: break;
    case MSG.LIST_RESP:
    case MSG.DATA_RESP:
    case MSG.ACK_RESP:
      if (fileHandler) fileHandler(t, payload);
      break;
  }
};

term.onData((d) => sendRaw(MSG.DATA, new TextEncoder().encode(d)));

window.addEventListener("resize", () => { fit.fit(); sendResize(); });
window.addEventListener("load", () => { fit.fit(); sendResize(); }); // late layout settle
document.getElementById("reconnect").onclick = () => location.reload();
document.getElementById("disconnect").onclick = () => ws.close(1000);

// ---- copy/paste (ported from WebSSHPage: buttons, context menu, shortcuts) ----
function copySelection() {
  if (term.hasSelection()) {
    navigator.clipboard.writeText(term.getSelection()).catch(() => {});
  }
}
async function pasteClipboard() {
  try {
    const text = await navigator.clipboard.readText();
    if (text && ws.readyState === 1) sendRaw(MSG.DATA, new TextEncoder().encode(text));
  } catch {
    term.writeln("\r\n\x1b[33m剪贴板读取失败，请检查浏览器剪贴板权限\x1b[0m");
  }
}
document.getElementById("copy").onclick = copySelection;
document.getElementById("paste").onclick = pasteClipboard;

const ctxmenu = document.getElementById("ctxmenu");
document.getElementById("term").addEventListener("contextmenu", (e) => {
  e.preventDefault();
  ctxmenu.style.display = "block";
  ctxmenu.style.left = e.clientX + "px";
  ctxmenu.style.top = e.clientY + "px";
});
document.addEventListener("click", () => { ctxmenu.style.display = "none"; });
document.getElementById("ctx-copy").onclick = copySelection;
document.getElementById("ctx-paste").onclick = pasteClipboard;

window.addEventListener("keydown", (e) => {
  if (e.ctrlKey && e.shiftKey && e.key === "C") { e.preventDefault(); copySelection(); }
  if (e.ctrlKey && e.shiftKey && e.key === "V") { e.preventDefault(); pasteClipboard(); }
});

// ===================== file panel (ported from FilePanel.tsx) =====================
const MARKDOWN_EXTS = new Set(["md", "markdown", "mdx"]);
const CODE_EXTENSIONS = new Set([
  "js", "jsx", "ts", "tsx", "mjs", "cjs",
  "py", "rb", "go", "rs", "java", "c", "h", "cpp", "hpp", "cc", "cxx",
  "cs", "swift", "kt", "scala", "ex", "exs", "erl", "hs", "ml", "r", "lua",
  "php", "perl", "pl",
  "sh", "bash", "zsh", "fish", "ps1", "bat", "cmd",
  "html", "htm", "css", "scss", "sass", "less", "vue", "svelte", "astro",
  "json", "yaml", "yml", "toml", "xml", "svg",
  "sql", "graphql", "gql",
  "tf", "hcl", "nomad",
  "dockerfile", "makefile", "cmake", "gradle",
  "ini", "conf", "cfg", "properties", "env", "service", "socket", "timer",
  "gitignore", "dockerignore", "editorconfig",
]);
const PLAIN_TEXT_EXTENSIONS = new Set(["txt", "log", "csv", "tsv", "readme", "license", "changelog"]);

function getPreviewMode(name) {
  const lower = name.toLowerCase();
  if (["makefile", "dockerfile", "vagrantfile", "gemfile", "rakefile", "procfile"].some((f) => lower === f)) return "code";
  if (["license", "readme", "changelog"].some((f) => lower === f)) return "markdown";
  const dot = lower.lastIndexOf(".");
  if (dot < 0) return null;
  const ext = lower.slice(dot + 1);
  if (MARKDOWN_EXTS.has(ext)) return "markdown";
  if (CODE_EXTENSIONS.has(ext)) return "code";
  if (PLAIN_TEXT_EXTENSIONS.has(ext)) return "text";
  return null;
}

// The UMD build exposes a namespace object ({markedHighlight: fn}); the ESM
// import mole uses binds the named export directly.
const markedHighlightFn = typeof markedHighlight === "function" ? markedHighlight : markedHighlight.markedHighlight;
marked.use(markedHighlightFn({
  langPrefix: "hljs language-",
  highlight(code, lang) {
    if (lang && hljs.getLanguage(lang)) return hljs.highlight(code, { language: lang }).value;
    return hljs.highlightAuto(code).value;
  },
}));
marked.use({ breaks: true, gfm: true });

function esc(s) { return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }
function formatSize(bytes) {
  if (bytes === 0) return "0 B";
  const k = 1024, sizes = ["B", "K", "M", "G"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return (bytes / Math.pow(k, i)).toFixed(i > 0 ? 1 : 0) + sizes[i];
}

const filesEl = document.getElementById("files");
const fpBody = document.getElementById("fp-body");
const fpTitle = document.getElementById("fp-title");
const fpLoading = document.getElementById("fp-loading");
const fpDropover = document.getElementById("fp-dropover");

const fp = {
  open: false, theme: "dark", fullscreen: false,
  pos: null, size: { w: 420, h: 500 },
  files: [], path: "/", home: "/", error: "",
  preview: null, previewLoading: false,
  uploading: false, progress: null,
  chunks: [], downloading: false, reading: null,
  readDec: null,
};

function applyPanelChrome() {
  // CSS variables live under #files.theme-dark/.theme-light — the prefix is
  // mandatory; without it every --fp-* var is undefined and the panel
  // renders with a transparent background (blends into the terminal).
  filesEl.className = "theme-" + fp.theme + (fp.fullscreen ? " fullscreen" : "");
  document.getElementById("fp-theme").textContent = fp.theme === "dark" ? "☀" : "☾";
  document.getElementById("fp-theme").title = fp.theme === "dark" ? "切换亮色主题" : "切换暗色主题";
  document.getElementById("fp-fullscreen").textContent = fp.fullscreen ? "⊡" : "⊞";
  document.getElementById("fp-fullscreen").title = fp.fullscreen ? "退出全屏" : "全屏";
  if (!fp.fullscreen) placePanel();
}
function placePanel() {
  if (!fp.pos) {
    fp.pos = {
      x: Math.max(20, window.innerWidth - fp.size.w - 40),
      y: Math.max(60, Math.round((window.innerHeight - fp.size.h) / 2)),
    };
  }
  filesEl.style.left = fp.pos.x + "px";
  filesEl.style.top = fp.pos.y + "px";
  filesEl.style.width = fp.size.w + "px";
  filesEl.style.height = fp.size.h + "px";
}

function closePanel() {
  // Hides the panel only. The WS file handler stays registered: nulling it
  // mid-transfer used to drop the completion ACK and permanently wedge
  // fp.uploading/downloading (mole recovers via component remount; our fp
  // state persists, so the handler must outlive the panel's visibility).
  fp.open = false;
  filesEl.style.display = "none";
  document.getElementById("toggle-files").classList.remove("active");
}

document.getElementById("toggle-files").onclick = () => {
  if (fp.open) { closePanel(); return; }
  fp.open = true;
  filesEl.style.display = "flex";
  applyPanelChrome();
  document.getElementById("toggle-files").classList.add("active");
  getList(fp.path);
};

document.getElementById("fp-close").onclick = closePanel;
document.getElementById("fp-theme").onclick = () => {
  fp.theme = fp.theme === "dark" ? "light" : "dark";
  applyPanelChrome(); render();
};
document.getElementById("fp-fullscreen").onclick = () => {
  fp.fullscreen = !fp.fullscreen;
  applyPanelChrome();
};

// title bar drag to move / corner drag to resize (same limits as mole)
document.getElementById("fp-head").addEventListener("mousedown", (e) => {
  if (e.target.closest("#fp-btns") || fp.fullscreen) return;
  e.preventDefault();
  const startX = e.clientX, startY = e.clientY, posX = fp.pos.x, posY = fp.pos.y;
  const onMove = (ev) => {
    fp.pos = { x: posX + ev.clientX - startX, y: posY + ev.clientY - startY };
    placePanel();
  };
  const onUp = () => {
    window.removeEventListener("mousemove", onMove);
    window.removeEventListener("mouseup", onUp);
  };
  window.addEventListener("mousemove", onMove);
  window.addEventListener("mouseup", onUp);
});
document.getElementById("fp-resize").addEventListener("mousedown", (e) => {
  if (fp.fullscreen) return;
  e.preventDefault();
  e.stopPropagation();
  const startX = e.clientX, startY = e.clientY, w = fp.size.w, h = fp.size.h;
  const onMove = (ev) => {
    fp.size = {
      w: Math.max(320, w + ev.clientX - startX),
      h: Math.max(300, h + ev.clientY - startY),
    };
    placePanel();
  };
  const onUp = () => {
    window.removeEventListener("mousemove", onMove);
    window.removeEventListener("mouseup", onUp);
  };
  window.addEventListener("mousemove", onMove);
  window.addEventListener("mouseup", onUp);
});

// nothing below this line runs before the WS is up; the panel starts hidden

// drag & drop upload onto the panel
// dragenter/leave fire on every child element; a depth counter keeps the
// overlay stable while the cursor moves across the panel's children
let dragDepth = 0;
filesEl.addEventListener("dragenter", (e) => { e.preventDefault(); e.stopPropagation(); dragDepth++; fpDropover.style.display = "flex"; });
filesEl.addEventListener("dragover", (e) => { e.preventDefault(); e.stopPropagation(); });
filesEl.addEventListener("dragleave", (e) => { e.preventDefault(); e.stopPropagation(); if (--dragDepth <= 0) { dragDepth = 0; fpDropover.style.display = "none"; } });
filesEl.addEventListener("drop", (e) => {
  e.preventDefault();
  e.stopPropagation();
  dragDepth = 0;
  fpDropover.style.display = "none";
  [...e.dataTransfer.files].forEach(uploadFile);
});

fileHandler = handleFileMessage; // resident: processes file frames even while the panel is hidden

function handleFileMessage(msgType, payload) {
  try {
    switch (msgType) {
      case MSG.LIST_RESP: {
        const resp = JSON.parse(dec.decode(payload));
        if (resp.files) fp.files = resp.files;
        if (resp.path) fp.path = resp.path;
        if (resp.home) fp.home = resp.home;
        fp.error = "";
        render();
        break;
      }
      case MSG.DATA_RESP:
        fp.chunks.push(new Uint8Array(payload));
        break;
      case MSG.ACK_RESP: {
        const ack = JSON.parse(dec.decode(payload));
        fp.previewLoading = false;
        // Ack payloads discriminate completions: read acks carry
        // mode:"read" + name, download acks carry name without mode, upload
        // acks carry only size. Matching on those (instead of requiring
        // chunks.length > 0) lets an empty file — which produces zero data
        // frames — still complete its preview/download instead of silently
        // doing nothing, and keeps unrelated acks from closing a transfer.
        if (ack.ok && ack.mode === "read" && fp.reading) {
          fp.preview = { name: fp.reading.name, content: fp.readDecResult(), mode: fp.reading.mode };
          fp.reading = null;
          fp.chunks = [];
          fp.downloading = false;
        } else if (ack.ok && ack.name && !ack.mode && fp.downloading && !fp.reading) {
          const blob = new Blob(fp.chunks.map((c) => c));
          const a = document.createElement("a");
          a.href = URL.createObjectURL(blob);
          a.download = ack.name;
          document.body.appendChild(a);
          a.click();
          a.remove();
          URL.revokeObjectURL(a.href);
          fp.chunks = [];
          fp.downloading = false;
        } else if (!ack.ok) {
          fp.error = ack.msg || "operation failed";
          fp.chunks = [];
          fp.downloading = false;
          fp.reading = null;
        }
        if (fp.uploading) { fp.uploading = false; fp.progress = null; }
        if (ack.ok) getList(fp.path);
        render();
        break;
      }
    }
  } catch {
    fp.error = "message parse error";
    render();
  }
}

function getList(path) { sendJSON(MSG.LIST_REQ, { path }); }

function navigateTo(path) {
  fp.path = path;
  fp.preview = null;
  getList(path);
}

function joinPath(name) { return fp.path === "/" ? "/" + name : fp.path + "/" + name; }

function rowClick(f) {
  if (f.is_dir) {
    navigateTo(joinPath(f.name));
  } else if (getPreviewMode(f.name)) {
    fp.reading = { name: f.name, mode: getPreviewMode(f.name) || "text" };
    fp.chunks = [];
    fp.downloading = true;
    fp.readDec = new TextDecoder(); // streaming decode fixes cross-chunk UTF-8
    fp.previewLoading = true;
    render();
    sendJSON(MSG.READ_REQ, { path: joinPath(f.name) });
  } else if (!fp.downloading) {
    fp.chunks = [];
    fp.downloading = true;
    render();
    sendJSON(MSG.DOWNLOAD_REQ, { path: joinPath(f.name) });
  }
}
// preview content via the streaming decoder accumulated across DATA chunks
fp.readDecResult = function () {
  const parts = [];
  for (const c of fp.chunks) parts.push(fp.readDec.decode(c, { stream: true }));
  parts.push(fp.readDec.decode());
  return parts.join("");
};

function goUp() {
  if (fp.path === "/") return;
  const parts = fp.path.split("/").filter(Boolean);
  parts.pop();
  navigateTo(parts.length === 0 ? "/" : "/" + parts.join("/"));
}

function handleDelete(f) {
  if (!confirm("确定删除 " + f.name + "？")) return;
  sendJSON(MSG.DELETE_REQ, { path: joinPath(f.name) });
}
function handleMkdir() {
  const name = prompt("目录名称：");
  if (!name) return;
  sendJSON(MSG.MKDIR_REQ, { path: joinPath(name) });
}

// Uploads are serialized through one chain (the server tracks a single
// in-flight upload per session). Chunks are awaited one by one —
// Blob.arrayBuffer() resolves asynchronously, so firing the sends up front
// would put UPLOAD_END on the wire before any data frame (0-byte files) —
// and the socket queue is kept shallow like mole's waitForBufferDrain.
let uploadChain = Promise.resolve();
function uploadFile(f) {
  uploadChain = uploadChain.then(() => doUpload(f)).catch(() => {});
}
function waitForBufferDrain(threshold) {
  if (ws.bufferedAmount <= threshold) return Promise.resolve();
  return new Promise((resolve) => {
    const check = () => {
      if (ws.bufferedAmount <= threshold || ws.readyState !== 1) resolve();
      else setTimeout(check, 20);
    };
    setTimeout(check, 20);
  });
}
async function doUpload(f) {
  if (fp.uploading) return;
  const dest = joinPath(f.name);
  fp.uploading = true;
  fp.progress = { loaded: 0, total: f.size, name: f.name };
  render();
  sendJSON(MSG.UPLOAD_REQ, { path: dest, size: f.size });
  const CHUNK = 32768;
  const BUFFER_THRESHOLD = CHUNK * 4;
  for (let off = 0; off < f.size; off += CHUNK) {
    if (ws.readyState !== 1) break;
    const buf = await f.slice(off, Math.min(off + CHUNK, f.size)).arrayBuffer();
    if (ws.readyState !== 1) break;
    await waitForBufferDrain(BUFFER_THRESHOLD);
    ws.send(frameRaw(MSG.UPLOAD_DATA, new Uint8Array(buf)));
    fp.progress = { loaded: Math.min(off + CHUNK, f.size), total: f.size, name: f.name };
    renderProgress();
  }
  sendJSON(MSG.UPLOAD_END, { path: dest });
}

// ---- rendering ----
function render() {
  if (!fp.open) return;
  fpTitle.textContent = fp.preview ? "预览: " + fp.preview.name : "SFTP 文件管理";
  if (fp.preview) renderPreview(); else renderList();
  fpLoading.style.display = fp.previewLoading ? "flex" : "none";
  renderProgress();
  renderError();
}
function renderProgress() {
  const el = document.getElementById("fp-progress");
  if (!el) return;
  const p = fp.progress;
  if (!p) { el.innerHTML = ""; return; }
  const pct = p.total ? Math.round(p.loaded / p.total * 100) : 0;
  el.innerHTML = esc(p.name) + ": " + formatSize(p.loaded) + " / " + formatSize(p.total) +
    " (" + pct + "%)<div class='bar'><div style='width:" + pct + "%'></div></div>";
}
function renderError() {
  const el = document.getElementById("fp-error");
  if (!el) return;
  el.textContent = fp.error || "";
  el.style.display = fp.error ? "block" : "none";
}
function renderList() {
  // rebuilding fp-body replaces the scrollable list element; keep the scroll
  // offset so refresh/delete/upload acks don't jump long listings to the top
  const savedScroll = (fpBody.querySelector(".fp-list") || {}).scrollTop || 0;
  const rows = fp.files.map((f) =>
    "<tr data-name=\"" + esc(f.name) + "\">" +
    "<td><span class='fname" + (f.is_dir ? " dir" : "") + "'>" +
    (f.is_dir ? "📁 " : "📄 ") + esc(f.name) + "</span></td>" +
    "<td>" + (f.is_dir ? "-" : formatSize(f.size)) + "</td>" +
    "<td>" + (f.mod_time ? esc(f.mod_time.slice(5, 16)) : "-") + "</td>" +
    "<td>" + (f.is_dir ? "" : "<button class='del' title=\"删除\">✕</button>") + "</td>" +
    "</tr>").join("");
  fpBody.innerHTML =
    "<div class='fp-pathbar'>" +
    "<button id='fp-up' title='上级目录'>⬆</button>" +
    "<input id='fp-path' value=\"" + esc(fp.path) + "\">" +
    "<button id='fp-home' title='主目录'>🏠</button>" +
    "<button id='fp-refresh' title='刷新'>🔄</button>" +
    "<label id='fp-upload' title='上传文件' class='" + (fp.uploading ? "busy" : "") + "'>⬆📁<input type='file' id='fp-file' " + (fp.uploading ? "disabled" : "") + " style='display:none'></label>" +
    "<button id='fp-mkdir' title='新建目录'>📁+</button>" +
    "</div>" +
    "<div id='fp-progress' class='fp-progress'></div>" +
    "<div id='fp-error' class='fp-error'></div>" +
    "<div class='fp-list'><table><thead><tr><th>名称</th><th style='width:64px'>大小</th><th style='width:110px'>修改时间</th><th style='width:32px'></th></tr></thead>" +
    "<tbody>" + (rows || "<tr><td colspan='4' class='fp-empty'>空目录</td></tr>") + "</tbody></table></div>" +
    "<div class='fp-hint'>拖放文件到面板上传 · 文本文件点击预览 · 其他文件点击下载</div>";
  document.getElementById("fp-up").onclick = goUp;
  document.getElementById("fp-home").onclick = () => navigateTo(fp.home);
  document.getElementById("fp-refresh").onclick = () => getList(fp.path);
  document.getElementById("fp-mkdir").onclick = handleMkdir;
  // the file input lives inside the label, so clicking the label opens the
  // picker natively; no explicit click() here (it would fire a second one)
  document.getElementById("fp-file").onchange = (e) => {
    [...e.target.files].forEach(uploadFile);
    e.target.value = "";
  };
  const pathInput = document.getElementById("fp-path");
  pathInput.onkeydown = (e) => { if (e.key === "Enter") navigateTo(pathInput.value); };
  fpBody.querySelectorAll("tr[data-name]").forEach((tr) => {
    const name = tr.dataset.name;
    const f = fp.files.find((x) => x.name === name);
    if (!f) return;
    tr.onclick = () => rowClick(f);
    const del = tr.querySelector(".del");
    if (del) {
      del.onclick = (e) => { e.stopPropagation(); handleDelete(f); };
    }
  });
  const listEl = fpBody.querySelector(".fp-list");
  if (listEl) listEl.scrollTop = savedScroll;
  renderProgress();
  renderError();
}

const CODE_LANG_MAP = {
  js: "javascript", mjs: "javascript", cjs: "javascript",
  ts: "typescript", tsx: "typescript", jsx: "javascript",
  py: "python", rb: "ruby", rs: "rust", kt: "kotlin",
  sh: "bash", zsh: "bash", fish: "shell",
  yml: "yaml", md: "markdown", htm: "html",
  h: "c", hpp: "cpp", cc: "cpp", cxx: "cpp",
};

function renderPreview() {
  const p = fp.preview;
  let view;
  if (p.mode === "markdown") {
    view = "<div class='fp-md file-preview-prose'>" +
      DOMPurify.sanitize(marked.parse(p.content)) + "</div>";
  } else if (p.mode === "code") {
    const ext = p.name.split(".").pop().toLowerCase();
    const lang = CODE_LANG_MAP[ext] || (hljs.getLanguage(ext) ? ext : null);
    const html = lang
      ? hljs.highlight(p.content, { language: lang }).value
      : hljs.highlightAuto(p.content).value;
    view = "<pre class='fp-code'><code class='hljs hljs-" + fp.theme + "'>" + html + "</code></pre>";
  } else {
    view = "<pre class='fp-text'></pre>";
  }
  fpBody.innerHTML =
    "<div class='fp-preview'>" +
    "<div class='fp-preview-bar'>" +
    "<button id='fp-back'>← 返回列表</button>" +
    "<span>" + esc(p.name) + "</span>" +
    "</div>" + view + "</div>";
  document.getElementById("fp-back").onclick = () => { fp.preview = null; render(); };
  if (p.mode === "text") fpBody.querySelector(".fp-text").textContent = p.content;
}
