"use strict";
const MSG = { DATA: 0x01, RESIZE: 0x02, KEEP: 0x03,
  LIST_REQ: 0x04, UPLOAD_REQ: 0x05, UPLOAD_DATA: 0x06, UPLOAD_END: 0x07,
  DOWNLOAD_REQ: 0x08, DELETE_REQ: 0x09, MKDIR_REQ: 0x0A, READ_REQ: 0x0B,
  LIST_RESP: 0x0C, DATA_RESP: 0x0D, ACK_RESP: 0x0E, READ_RESP: 0x0F };

const dec = new TextDecoder(); // one shared decoder for shell output + JSON frames

const qs = new URLSearchParams(location.search);
let token = qs.get("token") || localStorage.getItem("webssh_token") || "";
while (!token) {
  token = prompt("webssh token:") || "";
  if (token) localStorage.setItem("webssh_token", token);
}
let cwd = ".";
const statusEl = document.getElementById("status");
const filesEl = document.getElementById("files");
const cwdEl = document.getElementById("cwd");

const term = new Terminal({ cursorBlink: true, theme: { background: "#1e1e1e" } });
const fit = new FitAddon.FitAddon();
term.loadAddon(fit);
term.open(document.getElementById("term"));
fit.fit();

let dl = null; // active download accumulator {name, chunks}

const ws = new WebSocket(
  (location.protocol === "https:" ? "wss://" : "ws://") + location.host +
  "/webssh?token=" + encodeURIComponent(token));
ws.binaryType = "arraybuffer";

function frame(t, payload) {
  const body = payload ? new TextEncoder().encode(payload) : new Uint8Array(0);
  const buf = new Uint8Array(1 + body.length);
  buf[0] = t; buf.set(body, 1);
  return buf;
}
function frameRaw(t, bytes) {
  const buf = new Uint8Array(1 + bytes.length);
  buf[0] = t; buf.set(bytes, 1);
  return buf;
}
function send(t, payload) { if (ws.readyState === 1) ws.send(frame(t, payload)); }

ws.onopen = () => { statusEl.textContent = "online"; sendResize(); setInterval(() => send(MSG.KEEP), 30000); };
ws.onclose = (e) => { statusEl.textContent = "disconnected"; statusEl.className = "err"; };
ws.onerror = () => { statusEl.textContent = "error"; statusEl.className = "err"; };

ws.onmessage = (ev) => {
  const b = new Uint8Array(ev.data);
  const t = b[0], payload = b.subarray(1);
  switch (t) {
    case MSG.DATA: term.write(dec.decode(payload)); break;
    case MSG.KEEP: break;
    case MSG.LIST_RESP: renderList(JSON.parse(dec.decode(payload))); break;
    case MSG.DATA_RESP:
      if (dl) dl.chunks.push(payload); break;
    case MSG.ACK_RESP: handleAck(JSON.parse(dec.decode(payload))); break;
  }
};

function sendResize() { send(MSG.RESIZE, JSON.stringify({ cols: term.cols, rows: term.rows })); }
window.addEventListener("resize", () => { fit.fit(); sendResize(); });

// xterm data strings can contain non-ASCII: encode to UTF-8 bytes and send
// as a raw frame (frame()'s JSON-oriented path is not used here).
term.onData((d) => ws.send(frameRaw(MSG.DATA, new TextEncoder().encode(d))));

function handleAck(ack) {
  if (!ack.ok) { statusEl.textContent = "⚠ " + ack.msg; statusEl.className = "err"; return; }
  statusEl.className = "";
  // download/read completions carry name (+"size"); read acks also set
  // mode:"read" (server download acks have no mode field).
  if (dl && (ack.mode === "read" || ack.name)) {
    const blob = new Blob(dl.chunks);
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = dl.name; a.click();
    URL.revokeObjectURL(a.href);
    dl = null;
  }
  if (filesEl.style.display !== "none") listDir(cwd);
}

function listDir(path) { send(MSG.LIST_REQ, JSON.stringify({ path })); }

function renderList(resp) {
  cwd = resp.path; cwdEl.textContent = cwd;
  let rows = resp.files.map((f) =>
    `<tr data-name="${esc(f.name)}" data-dir="${f.is_dir}">
       <td>${f.is_dir ? "📁" : "📄"} ${esc(f.name)}</td>
       <td>${f.is_dir ? "-" : f.size}</td><td>${f.mode}</td><td>${f.mod_time}</td>
       <td><button class="dl">下载</button> <button class="del">删除</button></td>
     </tr>`).join("");
  filesEl.innerHTML = `<table><tr><th>名称</th><th>大小</th><th>模式</th><th>修改时间</th><th></th></tr>
    <tr><td colspan="5"><a href="#" id="parent">.. 上级</a></td></tr>${rows}</table>`;
  filesEl.querySelector("#parent").onclick = (e) => { e.preventDefault();
    listDir(cwd.split("/").slice(0, -1).join("/") || "/"); };
  filesEl.querySelectorAll("tr[data-name]").forEach((tr) => {
    const name = tr.dataset.name, dir = tr.dataset.dir === "true";
    const full = cwd === "/" ? "/" + name : cwd + "/" + name;
    if (dir) tr.ondblclick = () => listDir(full);
    tr.querySelector(".dl").onclick = () => { dl = { name, chunks: [] };
      send(MSG.DOWNLOAD_REQ, JSON.stringify({ path: full })); };
    tr.querySelector(".del").onclick = () => {
      if (confirm("删除 " + full + " ?")) send(MSG.DELETE_REQ, JSON.stringify({ path: full })); };
  });
}
function esc(s) { return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }

document.getElementById("toggle-files").onclick = () => {
  const show = filesEl.style.display !== "block";
  filesEl.style.display = show ? "block" : "none";
  if (show) listDir(cwd === "." ? "." : cwd);
};
document.getElementById("mkdir").onclick = () => {
  const p = prompt("新目录路径:"); if (p) send(MSG.MKDIR_REQ, JSON.stringify({ path: p }));
};
document.getElementById("up").onclick = () => {
  const input = document.createElement("input"); input.type = "file";
  input.onchange = () => uploadFile(input.files[0]);
  input.click();
};
function uploadFile(f) {
  const dest = (cwd === "/" ? "" : cwd) + "/" + f.name;
  send(MSG.UPLOAD_REQ, JSON.stringify({ path: dest, size: f.size }));
  const CHUNK = 32768;
  let off = 0;
  const pump = () => {
    while (off < f.size && ws.bufferedAmount < 4 * CHUNK) {
      const slice = f.slice(off, off + CHUNK);
      slice.arrayBuffer().then((ab) => ws.send(frameRaw(MSG.UPLOAD_DATA, new Uint8Array(ab))));
      off += CHUNK;
    }
    if (off < f.size) setTimeout(pump, 50);
    else ws.send(frame(MSG.UPLOAD_END));
  };
  pump();
}
// drag & drop upload onto file panel
const drop = document.getElementById("drop");
["dragover", "dragenter"].forEach((e) => document.addEventListener(e, (ev) => {
  if (filesEl.style.display === "block") { ev.preventDefault(); drop.classList.add("on"); }
}));
document.addEventListener("dragleave", () => drop.classList.remove("on"));
document.addEventListener("drop", (ev) => {
  if (filesEl.style.display === "block") { ev.preventDefault(); drop.classList.remove("on");
    [...ev.dataTransfer.files].forEach(uploadFile); }
});
