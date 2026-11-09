// Thin UI layer: all logic lives in Go. `api` is the Wails binding of internal/app.App.
"use strict";

const api = new Proxy({}, { get: (_, name) => (...args) => window.go.app.App[name](...args) });
const on = (event, fn) => window.runtime.EventsOn(event, fn);
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

// tab: the last of "characters" / "scenarios", the only tabs a chat can be opened from.
const state = {
  view: "characters", tab: "characters", chatId: null, chat: null,
  characters: [], scenarios: [], kbs: [], providers: [], settings: {}, chats: [],
  kbId: null, streaming: {},
};
const CHAT_TABS = ["characters", "scenarios"];

const ICON = {
  edit: `<svg viewBox="0 0 24 24"><path d="M4 20h4L19 9l-4-4L4 16z"/></svg>`,
  chat: `<svg viewBox="0 0 24 24"><path d="M4 5h16v11H9l-5 4z"/></svg>`,
  play: `<svg viewBox="0 0 24 24"><path d="M7 5l12 7-12 7z"/></svg>`,
};

// ---------- helpers ----------
function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
const IMAGE_LINE = /^\[Image: ([^|\]\n]+?)(?: \| ([^\]\n]*))?\]$/;
function formatMsg(s) {
  return s.split("\n").map(line => {
    const m = line.trim().match(IMAGE_LINE);
    if (m) return figureHTML(m[1], m[2]);
    return esc(line).replace(/\*([^*\n]+)\*/g, "<em>*$1*</em>");
  }).join("\n").replace(/<\/figure>\n/g, "</figure>");
}
// figureHTML is a placeholder for an illustration; loadFigures fills it in.
function figureHTML(src, caption = "") {
  return `<figure class="scene-img" data-src="${esc(src)}" title="${esc(caption)}"><div class="img-wait">🖼</div></figure>`;
}
const imageCache = new Map();
async function loadFigures(root = document) {
  for (const fig of root.querySelectorAll("figure.scene-img:not([data-loaded])")) {
    fig.dataset.loaded = "1";
    const src = fig.dataset.src;
    if (!imageCache.has(src)) imageCache.set(src, api.ChatImage(src).catch(() => null));
    const uri = await imageCache.get(src);
    if (!uri) { fig.innerHTML = `<div class="img-wait">⚠ Không tìm thấy ảnh</div>`; continue; }
    const img = new Image();
    img.src = uri;
    img.alt = fig.title;
    img.onclick = () => openModal(`<img src="${uri}" class="img-full" alt=""><p class="muted small">${esc(fig.title)}</p>
      <div class="modal-actions"><button value="cancel" class="primary">Đóng</button></div>`, () => {});
    fig.replaceChildren(img);
  }
}
new MutationObserver(() => loadFigures($("#messages"))).observe($("#messages"), { childList: true, subtree: true });
function toast(msg, isErr = false) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = "toast" + (isErr ? " err" : "");
  t.hidden = false;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => (t.hidden = true), isErr ? 7000 : 3000);
}
async function guard(fn) {
  try { return await fn(); } catch (e) { toast(String(e?.message ?? e), true); throw e; }
}
function avatarEl(c, cls = "avatar") {
  const d = document.createElement("div");
  d.className = cls;
  if (c?.avatar) d.style.backgroundImage = `url("${c.avatar}")`;
  else d.textContent = (c?.name || c?.title || "?").trim().charAt(0).toUpperCase();
  return d;
}
function charById(id) { return state.characters.find(c => c.id === id); }
function kbName(id) { return state.kbs.find(k => k.id === id)?.name ?? "?"; }

function openModal(html, onSubmit) {
  const dlg = $("#modal"), body = $("#modal-body");
  body.innerHTML = html;
  body.onsubmit = async e => {
    if (e.submitter?.value === "cancel") return;
    e.preventDefault();
    try { if ((await onSubmit?.(new FormData(body), e.submitter?.value)) !== false) dlg.close(); }
    catch (err) { toast(String(err?.message ?? err), true); }
  };
  dlg.showModal();
  return body;
}

function kbPicker(selected = []) {
  if (!state.kbs.length) return `<p class="muted small">Chưa có bộ truyện nào trong Thư viện.</p>`;
  return `<div class="kb-picks">${state.kbs.map(k =>
    `<label class="check"><input type="checkbox" name="kb" value="${k.id}" ${selected.includes(k.id) ? "checked" : ""}> ${esc(k.name)}</label>`).join("")}</div>`;
}

// ---------- providers ----------
async function loadProviders() {
  let all;
  [all, state.settings] = await Promise.all([api.ListProviders(), api.GetSettings()]);
  splitProviders(all);
}
// state.providers holds the language models (every model picker lists
// them); state.imageProviders the models that draw.
function splitProviders(all) {
  state.providers = all.filter(p => p.purpose !== "image");
  state.imageProviders = all.filter(p => p.purpose === "image");
}
function providerById(id) { return state.providers.find(p => p.id === id); }
// The provider a role falls back to when nothing specific is picked (mirrors App.resolve).
function defaultProvider(role) {
  return providerById(state.settings["provider." + role]) || providerById(state.settings["provider.chat"]);
}
function providerLabel(p) { return p ? `${p.name} · ${p.model}` : "chưa cấu hình"; }
// <option>s for a provider picker. The empty option means "use the default for role".
function providerOptions(selected, role, filter = () => true) {
  const def = role ? `<option value="">Mặc định (${esc(providerLabel(defaultProvider(role)))})</option>` : "";
  return def + state.providers.filter(filter).map(p =>
    `<option value="${p.id}" ${p.id === selected ? "selected" : ""}>${esc(providerLabel(p))}</option>`).join("");
}

// ---------- navigation ----------
function show(view) {
  state.view = view;
  if (CHAT_TABS.includes(view)) state.tab = view;
  $$(".view").forEach(v => (v.hidden = v.id !== "view-" + view));
  const tab = view === "chat" ? state.tab : view;
  $$("nav button").forEach(b => b.classList.toggle("active", b.dataset.view === tab));
  // Recent chats belong to the character/scenario tabs; other tabs stay uncluttered.
  $("#recent").hidden = !(CHAT_TABS.includes(view) || view === "chat");
  if (view !== "chat") { state.chatId = null; state.chat = null; $("#chat-drawer").hidden = true; }
  if (CHAT_TABS.includes(view)) renderChatList(); // openChat renders it for the chat view
  ({ characters: loadCharacters, scenarios: loadScenarios, library: loadLibrary, settings: loadSettings }[view])?.();
}
$$("nav button").forEach(b => (b.onclick = () => show(b.dataset.view)));

$("#create-btn").onclick = e => { e.stopPropagation(); $("#create-menu").hidden = !$("#create-menu").hidden; };
document.addEventListener("click", () => ($("#create-menu").hidden = true));
$$("#create-menu button").forEach(b => (b.onclick = async () => {
  $("#create-menu").hidden = true;
  const what = b.dataset.create;
  if (what === "play") { show("scenarios"); quickPlayDialog(); }
  if (what === "character") { show("characters"); editCharacter(); }
  if (what === "scenario") { show("scenarios"); editScenario(); }
  if (what === "kb") { show("library"); editKB(); }
}));

// ---------- characters ----------
async function loadCharacters() {
  [state.characters, state.kbs, state.chats] = await Promise.all([api.ListCharacters(), api.ListKnowledgeBases(), api.ListChats()]);
  renderContinue();
  renderCharacterGrid();
}

function renderContinue() {
  const row = $("#continue-row");
  row.innerHTML = "";
  const seen = new Set();
  for (const c of state.chats) {
    const ch = charById(c.characterId);
    if (!ch || seen.has(ch.id) || seen.size >= 8) continue;
    seen.add(ch.id);
    const el = document.createElement("div");
    el.className = "continue-item";
    el.append(avatarEl(ch));
    el.insertAdjacentHTML("beforeend", `<div class="ci-text"><div><b>${esc(ch.name)}</b></div><div class="muted small">${esc(c.title || ch.tagline)}</div></div>`);
    el.onclick = () => openChat(c.id);
    row.append(el);
  }
  $("#continue").hidden = !seen.size;
}

function renderCharacterGrid() {
  const grid = $("#character-grid");
  const q = $("#char-search").value.trim().toLowerCase();
  grid.innerHTML = "";
  if (!state.characters.length) {
    grid.innerHTML = `<div class="empty">Chưa có nhân vật. Thêm truyện vào <b>Thư viện</b>, rồi bấm <b>+ Tạo → Nhân vật</b> để AI dựng nhân vật từ truyện.</div>`;
    return;
  }
  const list = state.characters.filter(c => !q || (c.name + " " + c.tagline + " " + c.description).toLowerCase().includes(q));
  if (!list.length) grid.innerHTML = `<div class="empty">Không tìm thấy nhân vật nào.</div>`;
  for (const c of list) {
    const n = state.chats.filter(x => x.characterId === c.id).length;
    const card = document.createElement("div");
    card.className = "ccard";
    card.append(avatarEl(c, "ccard-img"));
    card.insertAdjacentHTML("beforeend", `<div class="ccard-body">
        <div class="ccard-name">${esc(c.name)}</div>
        <div class="ccard-by">${c.kbIds.length ? "từ " + esc(c.kbIds.map(kbName).join(", ")) : "nhân vật tự do"}</div>
        <div class="ccard-desc">${esc(c.tagline || c.description)}</div>
        <div class="ccard-meta">${ICON.chat}${n ? `${n} cuộc trò chuyện` : "Chưa trò chuyện"}</div>
      </div>
      <div class="card-actions"><button class="icon-btn" data-a="edit" title="Sửa">${ICON.edit}</button></div>`);
    card.onclick = e => {
      if (e.target.closest("[data-a=edit]")) return editCharacter(c);
      // Like c.ai: continue the latest conversation, or start the first one.
      const last = state.chats.find(x => x.characterId === c.id);
      last ? openChat(last.id) : startChatDialog({ character: c });
    };
    grid.append(card);
  }
}
$("#char-search").oninput = renderCharacterGrid;

async function editCharacter(c = { kbIds: [] }, after = loadCharacters) {
  await loadProviders();
  if (!state.kbs.length) state.kbs = await api.ListKnowledgeBases();
  const f = (name, label, rows = 0) => rows
    ? `<label>${label}<textarea name="${name}" rows="${rows}">${esc(c[name])}</textarea></label>`
    : `<label>${label}<input name="${name}" value="${esc(c[name])}"></label>`;
  const body = openModal(`
    <h2>${c.id ? "Sửa nhân vật" : "Tạo nhân vật"}</h2>
    <label>Lấy kiến thức từ bộ truyện</label>${kbPicker(c.kbIds)}
    <div class="gen-box">
      <div class="muted small">Tự động tạo từ thư viện (dùng RAG để tìm hồ sơ nhân vật trong truyện)</div>
      <div class="gen-row" style="margin-top:8px"><input id="gen-hint" placeholder="Gợi ý thêm (vd: giai đoạn sau chương 50, tính cách lạnh lùng hơn…)"></div>
      <div class="gen-row" style="margin-top:8px">
        <span class="muted small">Model</span>
        <select id="gen-provider" style="flex:1;max-width:none">${providerOptions("", "summarize")}</select>
        <button type="button" id="gen-btn">✨ Tạo</button>
      </div>
    </div>
    <div style="display:flex;gap:14px;align-items:center">
      <div id="av-preview"></div>
      <label style="flex:1">Ảnh đại diện<input type="file" id="av-file" accept="image/*"></label>
    </div>
    ${f("name", "Tên")}${f("tagline", "Câu giới thiệu ngắn")}${f("description", "Mô tả", 4)}
    ${f("personality", "Tính cách", 3)}${f("speakingStyle", "Cách nói chuyện", 2)}
    ${f("greeting", "Lời chào mở đầu", 3)}${f("exampleDialogue", "Hội thoại mẫu ({{user}} / {{char}})", 4)}
    <div class="modal-actions">
      ${c.id ? `<button value="delete" class="danger ghost" style="margin-right:auto">Xoá</button>` : ""}
      <button value="cancel">Huỷ</button><button value="save" class="primary">Lưu</button>
    </div>`,
    async (fd, action) => {
      if (action === "delete") {
        await api.DeleteCharacter(c.id);
      } else {
        const out = { ...c, kbIds: fd.getAll("kb"), avatar: c.avatar || "" };
        for (const k of ["name", "tagline", "description", "personality", "speakingStyle", "greeting", "exampleDialogue"]) out[k] = fd.get(k);
        if (!out.name.trim()) { toast("Cần nhập tên", true); return false; }
        await api.SaveCharacter(out);
      }
      after();
    });

  const preview = () => { const p = $("#av-preview", body); p.innerHTML = ""; p.append(avatarEl(c)); p.firstChild.style.cssText += "width:64px;height:64px"; };
  preview();
  $("#av-file", body).onchange = async e => { c.avatar = await resizeImage(e.target.files[0], 256); preview(); };
  $("#gen-btn", body).onclick = async () => {
    const kbIds = $$("input[name=kb]:checked", body).map(i => i.value);
    const name = $("input[name=name]", body).value.trim();
    if (!kbIds.length || !name) return toast("Chọn bộ truyện và nhập tên nhân vật trước", true);
    const btn = $("#gen-btn", body);
    btn.disabled = true; btn.textContent = "Đang tạo…";
    try {
      const g = await guard(() => api.GenerateCharacter(kbIds, name, $("#gen-hint", body).value, $("#gen-provider", body).value));
      for (const k of ["name", "tagline", "description", "personality", "speakingStyle", "greeting", "exampleDialogue"]) {
        const el = $(`[name=${k}]`, body);
        if (g[k]) el.value = g[k];
      }
      toast("Đã tạo bản nháp — chỉnh sửa rồi lưu");
    } finally { btn.disabled = false; btn.textContent = "✨ Tạo"; }
  };
}

function resizeImage(file, size) {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => {
      const cv = document.createElement("canvas");
      const s = Math.min(img.width, img.height);
      cv.width = cv.height = size;
      cv.getContext("2d").drawImage(img, (img.width - s) / 2, (img.height - s) / 2, s, s, 0, 0, size, size);
      resolve(cv.toDataURL("image/jpeg", 0.85));
    };
    img.onerror = reject;
    img.src = URL.createObjectURL(file);
  });
}

// rolePicker lists the roles the player can take: each cast member, or their own character.
function rolePicker(cast) {
  return cast.map((m, i) => `<label class="role"><input type="radio" name="role" value="${i}">
      <div><b>${esc(m.name)}</b><div class="muted small">${esc(m.description)}</div></div></label>`).join("") +
    `<label class="role"><input type="radio" name="role" value="own" ${cast.length ? "" : "checked"}>
      <div><b>Tự tạo nhân vật</b><div class="muted small">Một nhân vật mới của riêng bạn bước vào câu chuyện.</div></div></label>`;
}
// wireRoles makes picking a cast member fill in the role; "own" clears it for the player to write.
function wireRoles(body, cast) {
  $$("input[name=role]", body).forEach(r => (r.onchange = () => {
    const m = cast[r.value];
    $("[name=personaName]", body).value = m?.name ?? "";
    $("[name=personaDesc]", body).value = m?.description ?? "";
    if (!m) $("[name=personaName]", body).focus();
  }));
}

// startChatDialog opens a new conversation. From a character: one-on-one chat
// (pick a scenario). From a scenario: either play a role in it (a cast member or
// your own character) with the model narrating everyone else, or chat one-on-one
// with a character card inside it.
async function startChatDialog({ character, scenario }) {
  [state.scenarios, state.characters] = await Promise.all([api.ListScenarios(), api.ListCharacters()]);
  await loadProviders();
  if (character) return characterChatDialog(character);
  const cast = scenario.cast ?? [];
  let mode = "scene";
  const body = openModal(`
    <h2>${esc(scenario.title)}</h2>
    <div class="seg" id="mode-seg">
      <button type="button" data-mode="scene" class="on">Hoá thân vào truyện</button>
      <button type="button" data-mode="character">Chat với một nhân vật</button>
    </div>
    <div id="mode-scene">
      <p class="muted small">Bạn vào vai một nhân vật, AI làm người dẫn truyện và điều khiển thế giới cùng tất cả nhân vật còn lại.</p>
      <div class="role-list">${rolePicker(cast)}</div>
      <label>Tên nhân vật của bạn<input name="personaName" placeholder="Bạn"></label>
      <label>Mô tả nhân vật của bạn<textarea name="personaDesc" rows="3" placeholder="Ngoại hình, thân phận, năng lực, mục đích khi bước vào câu chuyện…"></textarea></label>
    </div>
    <div id="mode-character" hidden>
      ${state.characters.length
        ? `<label>Nhân vật<select name="character">${state.characters.map(c => `<option value="${c.id}">${esc(c.name)}</option>`).join("")}</select></label>
           <label>Tên của bạn trong truyện<input name="charPersonaName" placeholder="Bạn"></label>
           <label>Mô tả nhân vật của bạn (tuỳ chọn)<textarea name="charPersonaDesc" rows="3"></textarea></label>`
        : `<p class="muted">Chưa có thẻ nhân vật nào — tạo ở tab Nhân vật trước.</p>`}
    </div>
    <label>Model<select name="provider">${providerOptions("", "chat")}</select></label>
    <div class="modal-actions"><button value="cancel">Huỷ</button><button class="primary">Bắt đầu</button></div>`,
    async fd => {
      let chat;
      if (mode === "scene") {
        const name = fd.get("personaName").trim();
        if (!name) { toast("Chọn một vai hoặc nhập tên nhân vật của bạn", true); return false; }
        chat = await api.CreateChat({ mode: "scene", scenarioId: scenario.id, providerId: fd.get("provider"),
          personaName: name, personaDesc: fd.get("personaDesc") });
      } else {
        if (!fd.get("character")) { toast("Chưa có thẻ nhân vật", true); return false; }
        chat = await api.CreateChat({ characterId: fd.get("character"), scenarioId: scenario.id, providerId: fd.get("provider"),
          personaName: fd.get("charPersonaName"), personaDesc: fd.get("charPersonaDesc") });
      }
      openChat(chat.id);
    });

  $$("#mode-seg button", body).forEach(b => (b.onclick = () => {
    mode = b.dataset.mode;
    $$("#mode-seg button", body).forEach(x => x.classList.toggle("on", x === b));
    $("#mode-scene", body).hidden = mode !== "scene";
    $("#mode-character", body).hidden = mode !== "character";
  }));
  wireRoles(body, cast);
}

function characterChatDialog(character) {
  openModal(`
    <h2>Chat với ${esc(character.name)}</h2>
    <label>Bối cảnh<select name="scenario"><option value="">— Không dùng —</option>
      ${state.scenarios.map(s => `<option value="${s.id}">${esc(s.title)}</option>`).join("")}</select></label>
    <label>Model<select name="provider">${providerOptions("", "chat")}</select></label>
    <label>Tên của bạn trong truyện<input name="personaName" placeholder="Bạn"></label>
    <label>Mô tả nhân vật của bạn (tuỳ chọn)<textarea name="personaDesc" rows="3"></textarea></label>
    <div class="modal-actions"><button value="cancel">Huỷ</button><button class="primary">Bắt đầu</button></div>`,
    async fd => {
      const chat = await api.CreateChat({ characterId: character.id, scenarioId: fd.get("scenario"), providerId: fd.get("provider"),
        personaName: fd.get("personaName"), personaDesc: fd.get("personaDesc") });
      openChat(chat.id);
    });
}

// quickPlayDialog starts a scene straight from the library: pick the work and
// a role (the cast is every character the work has a profile for) and go. No
// scenario to write; the narrator opens the story from what it retrieves.
async function quickPlayDialog(kbIds = []) {
  if (!state.kbs.length) state.kbs = await api.ListKnowledgeBases();
  if (!state.kbs.length) return toast("Thêm một bộ truyện ở Thư viện trước", true);
  await loadProviders();
  if (!kbIds.length) kbIds = [state.kbs[0].id];
  let cast = [];
  const body = openModal(`
    <h2>▶ Chơi ngay</h2>
    <p class="muted small">Không cần tạo bối cảnh: chọn truyện và vai, người dẫn truyện tự mở đầu câu chuyện từ dữ liệu trong truyện và điều khiển mọi nhân vật còn lại.</p>
    <label>Bộ truyện</label>${kbPicker(kbIds)}
    <label>Bắt đầu từ đâu? (tuỳ chọn)<input name="start" placeholder="Vd: ngày đầu ở trung tâm kiểm tra. Để trống để AI tự chọn"></label>
    <label>Vai của bạn</label>
    <div class="role-list" id="qp-roles"></div>
    <label>Tên nhân vật của bạn<input name="personaName" placeholder="Bạn"></label>
    <label>Mô tả nhân vật của bạn<textarea name="personaDesc" rows="3" placeholder="Ngoại hình, thân phận, năng lực, mục đích khi bước vào câu chuyện…"></textarea></label>
    <label>Model<select name="provider">${providerOptions("", "chat")}</select></label>
    <div class="modal-actions"><button value="cancel">Huỷ</button><button class="primary">Bắt đầu</button></div>`,
    async fd => {
      const ids = fd.getAll("kb");
      if (!ids.length) { toast("Chọn ít nhất một bộ truyện", true); return false; }
      const name = fd.get("personaName").trim();
      if (!name) { toast("Chọn một vai hoặc nhập tên nhân vật của bạn", true); return false; }
      const chat = await api.QuickPlay({ kbIds: ids, start: fd.get("start"), personaName: name,
        personaDesc: fd.get("personaDesc"), providerId: fd.get("provider") });
      state.tab = "scenarios";
      openChat(chat.id);
    });
  const roles = $("#qp-roles", body);
  const loadRoles = async () => {
    const ids = $$("input[name=kb]:checked", body).map(i => i.value);
    roles.innerHTML = `<p class="muted small">Đang tải nhân vật…</p>`;
    cast = ids.length ? await guard(() => api.KBCharacters(ids)) : [];
    roles.innerHTML = rolePicker(cast);
    wireRoles(body, cast);
  };
  $$("input[name=kb]", body).forEach(i => (i.onchange = loadRoles));
  loadRoles();
}
$("#quick-play").onclick = () => quickPlayDialog();

// ---------- scenarios ----------
async function loadScenarios() {
  [state.scenarios, state.kbs] = await Promise.all([api.ListScenarios(), api.ListKnowledgeBases()]);
  const list = $("#scenario-list");
  const saved = state.scenarios.filter(s => !s.quick); // "Chơi ngay" scenes live in the chat list only
  list.innerHTML = saved.length ? "" : `<div class="empty">Chưa có bối cảnh lưu sẵn. Bấm <b>▶ Chơi ngay</b> để vào truyện luôn, hoặc <b>+ Tạo bối cảnh</b> để soạn một cảnh và chơi lại nhiều lần.</div>`;
  for (const s of saved) {
    const card = document.createElement("div");
    card.className = "ccard";
    card.append(avatarEl({ name: s.title }, "ccard-img ccard-scene"));
    const names = s.cast.map(m => m.name);
    card.insertAdjacentHTML("beforeend", `<div class="ccard-body">
        <div class="ccard-name">${esc(s.title)}</div>
        <div class="ccard-by">${s.kbIds.length ? "từ " + esc(s.kbIds.map(kbName).join(", ")) : "bối cảnh tự do"}</div>
        <div class="ccard-desc">${esc(s.setting)}</div>
        <div class="ccard-meta">${ICON.play}${names.length ? esc(names.slice(0, 4).join(", ")) + (names.length > 4 ? "…" : "") : "Bấm để nhập vai"}</div>
      </div>
      <div class="card-actions"><button class="icon-btn" data-a="edit" title="Sửa">${ICON.edit}</button></div>`);
    card.onclick = e => (e.target.closest("[data-a=edit]") ? editScenario(s) : startChatDialog({ scenario: s }));
    list.append(card);
  }
}

async function editScenario(s = { kbIds: [], cast: [] }) {
  await loadProviders();
  if (!state.kbs.length) state.kbs = await api.ListKnowledgeBases();
  const body = openModal(`
    <h2>${s.id ? "Sửa bối cảnh" : "Tạo bối cảnh"}</h2>
    <label>Lấy kiến thức từ bộ truyện</label>${kbPicker(s.kbIds)}
    <div class="gen-box">
      <div class="muted small">Nhập ý tưởng bối cảnh, AI dựng bối cảnh và trích xuất các nhân vật có mặt từ bộ truyện</div>
      <div class="gen-row" style="margin-top:8px"><input id="gen-hint" placeholder="Vd: đêm ma tộc tấn công làng Thanh Vân"></div>
      <div class="gen-row" style="margin-top:8px">
        <span class="muted small">Model</span>
        <select id="gen-provider" style="flex:1;max-width:none">${providerOptions("", "summarize")}</select>
        <button type="button" id="gen-btn">✨ Tạo</button>
      </div>
    </div>
    <label>Tiêu đề<input name="title" value="${esc(s.title)}"></label>
    <label>Bối cảnh (thời gian, địa điểm, tình huống)<textarea name="setting" rows="5">${esc(s.setting)}</textarea></label>
    <label>Đoạn mở đầu<textarea name="opening" rows="4">${esc(s.opening)}</textarea></label>
    <div class="card-head" style="margin-top:16px">
      <h2 style="margin:0">Nhân vật trong bối cảnh</h2>
      <div><button type="button" id="cast-extract" title="Đọc bộ truyện để tìm nhân vật hợp với bối cảnh đã viết">✨ Trích xuất từ truyện</button>
        <button type="button" id="cast-add">+ Thêm</button></div>
    </div>
    <p class="muted small">Người chơi có thể chọn hoá thân vào một trong các nhân vật này; AI điều khiển những người còn lại.</p>
    <div id="cast-list" class="cast-list"></div>
    <div class="modal-actions">
      ${s.id ? `<button value="delete" class="danger ghost" style="margin-right:auto">Xoá</button>` : ""}
      <button value="cancel">Huỷ</button><button value="save">Lưu</button><button value="play" class="primary">Lưu & nhập vai</button>
    </div>`,
    async (fd, action) => {
      if (action === "delete") { await api.DeleteScenario(s.id); loadScenarios(); return; }
      if (!fd.get("title").trim()) { toast("Cần nhập tiêu đề", true); return false; }
      const saved = await api.SaveScenario({ ...s, title: fd.get("title"), setting: fd.get("setting"), opening: fd.get("opening"),
        kbIds: fd.getAll("kb"), cast: readCast() });
      loadScenarios();
      if (action === "play") setTimeout(() => startChatDialog({ scenario: saved })); // after this modal closes
    });

  const castBox = $("#cast-list", body);
  const addCast = (m = {}) => {
    const row = document.createElement("div");
    row.className = "cast-row";
    row.innerHTML = `<div class="gen-row"><input class="cast-name" placeholder="Tên nhân vật" value="${esc(m.name)}">
      <button type="button" class="ghost danger" title="Bỏ">✕</button></div>
      <textarea class="cast-desc" rows="2" placeholder="Họ là ai, tính cách, mục đích trong cảnh này…">${esc(m.description)}</textarea>`;
    row.querySelector("button").onclick = () => row.remove();
    castBox.append(row);
  };
  const setCast = cast => { castBox.innerHTML = ""; (cast ?? []).forEach(m => addCast(m)); };
  const readCast = () => $$(".cast-row", castBox)
    .map(r => ({ name: $(".cast-name", r).value.trim(), description: $(".cast-desc", r).value.trim() }))
    .filter(m => m.name);
  setCast(s.cast);
  $("#cast-add", body).onclick = () => { addCast(); $$(".cast-name", castBox).at(-1).focus(); };

  const busy = async (btn, label, fn) => {
    const text = btn.textContent;
    btn.disabled = true; btn.textContent = label;
    try { await fn(); } finally { btn.disabled = false; btn.textContent = text; }
  };
  const pickedKBs = () => $$("input[name=kb]:checked", body).map(i => i.value);
  $("#gen-btn", body).onclick = e => {
    const kbIds = pickedKBs();
    if (!kbIds.length) return toast("Chọn ít nhất một bộ truyện", true);
    busy(e.target, "Đang tạo…", async () => {
      const g = await guard(() => api.GenerateScenario(kbIds, $("#gen-hint", body).value, $("#gen-provider", body).value));
      for (const k of ["title", "setting", "opening"]) if (g[k]) $(`[name=${k}]`, body).value = g[k];
      if (g.cast?.length) setCast(g.cast);
      toast(`Đã tạo bối cảnh với ${g.cast?.length ?? 0} nhân vật — chỉnh sửa rồi lưu`);
    });
  };
  $("#cast-extract", body).onclick = e => {
    const kbIds = pickedKBs();
    const title = $("[name=title]", body).value.trim(), setting = $("[name=setting]", body).value.trim();
    if (!kbIds.length || !setting) return toast("Chọn bộ truyện và viết phần bối cảnh trước", true);
    busy(e.target, "Đang trích xuất…", async () => {
      const cast = await guard(() => api.ExtractCast(kbIds, title, setting, $("#gen-provider", body).value));
      setCast(cast);
      toast(`Tìm thấy ${cast.length} nhân vật`);
    });
  };
}
$("#new-scenario").onclick = () => editScenario();

// ---------- library ----------
const BUSY = ["queued", "extracting", "reading", "summarizing", "embedding"];
const STATUS_LABEL = { queued: "Chờ", extracting: "Đọc file", reading: "Đọc tranh", read: "Chờ embedding", embedding: "Embedding", embedded: "Chờ lập bản đồ", summarizing: "Lập bản đồ", done: "Xong", error: "Lỗi" };
// Suggested embedding model per provider kind (all served on /v1/embeddings).
const EMBED_MODELS = {
  openai: "text-embedding-3-small",
  openrouter: "openai/text-embedding-3-small",
  nvidia: "nvidia/llama-3.2-nv-embedqa-1b-v2",
  local: "nomic-embed-text",
};

async function loadLibrary() {
  state.kbs = await api.ListKnowledgeBases();
  const list = $("#kb-list");
  list.innerHTML = state.kbs.length ? "" : `<p class="muted">Chưa có bộ truyện.</p>`;
  for (const k of state.kbs) {
    const el = document.createElement("div");
    el.className = "kb-item" + (k.id === state.kbId ? " active" : "");
    el.innerHTML = `<div>${esc(k.name)}</div><div class="muted small">${esc(k.description)}</div>`;
    el.onclick = () => { state.kbId = k.id; loadLibrary(); };
    list.append(el);
  }
  if (state.kbId && state.kbs.some(k => k.id === state.kbId)) renderKB(state.kbs.find(k => k.id === state.kbId));
  else $("#kb-detail").innerHTML = `<p class="muted">Chọn hoặc tạo một bộ truyện.</p>`;
}

async function renderKB(kb) {
  const [sources] = await Promise.all([api.ListSources(kb.id), loadProviders()]);
  const embedders = state.providers.filter(p => p.kind !== "anthropic");
  const d = $("#kb-detail");
  d.innerHTML = `
    <div class="card">
      <div class="card-head"><h2>${esc(kb.name)}</h2>
        <div><button id="kb-play" class="primary">▶ Chơi ngay</button> <button id="kb-resum" title="Lập lại bản đồ truyện và hồ sơ nhân vật từ phần đầu">↻ Lập lại bản đồ</button> <button id="kb-edit">Sửa</button> <button id="kb-del" class="danger ghost">Xoá</button></div></div>
      <div class="gen-row">
        <select id="comic-mode" style="flex:1;max-width:none">
          <option value="vision">Truyện tranh: đọc bằng vision LLM (chính xác, tốn token 1 lần)</option>
          <option value="embed">Truyện tranh: chỉ embed ảnh (miễn phí, không tóm tắt được)</option>
          <option value="both">Truyện tranh: cả hai</option>
        </select>
        <button class="primary" id="add-files">+ Thêm tài liệu</button>
      </div>
      <p class="muted small">Hỗ trợ PDF, EPUB, CBZ, TXT, MD, ảnh. Cả bộ đi qua ba bước, bước sau chờ mọi tài liệu xong bước trước: đọc (vision đọc từng trang tranh, mỗi lần một trang) → embedding → lập bản đồ truyện theo thứ tự chương. Tài liệu thêm sau được nối tiếp vào bản đồ; nhiều file chọn cùng lúc được xếp theo tên (Chương 2 trước Chương 10).</p>
      <table><thead><tr><th>Tài liệu</th><th>Trạng thái</th><th></th></tr></thead><tbody id="src-rows"></tbody></table>
    </div>
    <div class="card">
      <div class="card-head"><h2>Model xử lý & embedding</h2><button class="primary" id="kb-models-save">Lưu</button></div>
      <p class="muted small">Áp dụng cho các lần xử lý tài liệu của bộ truyện này.</p>
      <div class="row2">
        <label>Tóm tắt & rút hồ sơ<select id="kb-llm">${providerOptions(kb.llmProvider, "summarize")}</select></label>
        <label>Đọc truyện tranh (vision)<select id="kb-vision">${providerOptions(kb.visionProvider, "vision", p => p.vision)}</select></label>
        <label>Embedding<select id="kb-embed">
          <option value="">Local · ${esc(state.settings["embed.text_model"])} (miễn phí, chạy trên máy)</option>
          ${embedders.map(p => `<option value="${p.id}" ${p.id === kb.embedProvider ? "selected" : ""}>${esc(p.name)} (API)</option>`).join("")}
        </select></label>
        <label id="kb-embed-model-row">Model embedding<div class="gen-row">
          <input id="kb-embed-model" list="kb-embed-models" value="${esc(kb.embedModel)}"><button type="button" id="kb-embed-load">Tải danh sách</button></div></label>
      </div>
      <datalist id="kb-embed-models"></datalist>
      <p class="muted small">Đổi embedding sẽ embedding lại toàn bộ văn bản đã lưu sang model mới (không gọi lại LLM). Claude không có API embedding.</p>
    </div>
    <div class="card"><h2>Bản đồ truyện (tự động)</h2><p class="muted small">Mỗi chương vài câu, theo thứ tự truyện. Khi chat, model đọc bản đồ này rồi tự tra chi tiết (hồ sơ nhân vật, nội dung và lời thoại từng chương).</p><div class="overview">${esc(kb.overview) || '<span class="muted">Sẽ có sau khi xử lý xong tài liệu.</span>'}</div></div>
    <div class="card"><h2>Thử truy xuất RAG</h2>
      <div class="gen-row"><input id="kb-q" placeholder="Hỏi gì đó về truyện…"><button id="kb-search">Tìm</button></div>
      <div id="kb-hits"></div></div>`;
  const rows = $("#src-rows", d);
  rows.innerHTML = sources.length ? "" : `<tr><td colspan="3" class="muted">Chưa có tài liệu.</td></tr>`;
  for (const s of sources) rows.append(sourceRow(s));

  const embedSel = $("#kb-embed", d), embedModel = $("#kb-embed-model", d);
  const syncEmbed = () => {
    const p = providerById(embedSel.value);
    $("#kb-embed-model-row", d).hidden = !p;
    if (p && !embedModel.value) embedModel.value = EMBED_MODELS[p.kind] ?? "";
  };
  embedSel.onchange = () => { embedModel.value = ""; syncEmbed(); };
  syncEmbed();
  $("#kb-embed-load", d).onclick = async () => {
    const models = await guard(() => api.ListProviderModels(embedSel.value));
    const embedLike = models.filter(m => /embed/i.test(m));
    $("#kb-embed-models", d).innerHTML = (embedLike.length ? embedLike : models).map(m => `<option value="${esc(m)}">`).join("");
    toast(`${embedLike.length || models.length} model — gõ để lọc`);
  };
  $("#kb-models-save", d).onclick = async e => {
    const next = {
      ...kb, llmProvider: $("#kb-llm", d).value, visionProvider: $("#kb-vision", d).value,
      embedProvider: embedSel.value, embedModel: embedSel.value ? embedModel.value.trim() : "",
    };
    if (next.embedProvider && !next.embedModel) return toast("Nhập model embedding", true);
    const reembed = next.embedProvider !== kb.embedProvider || next.embedModel !== kb.embedModel;
    const btn = e.target;
    btn.disabled = true;
    btn.textContent = reembed && sources.length ? "Đang embedding lại…" : "Đang lưu…";
    try {
      await guard(() => api.SaveKnowledgeBase(next));
      toast(reembed && sources.length ? "Đã chuyển sang model embedding mới" : "Đã lưu");
      loadLibrary();
    } finally { btn.disabled = false; btn.textContent = "Lưu"; }
  };

  $("#add-files", d).onclick = async () => {
    const paths = await guard(() => api.PickFiles());
    if (!paths?.length) return;
    await guard(() => api.AddSources(kb.id, paths, $("#comic-mode", d).value));
    toast(`Đã thêm ${paths.length} tài liệu vào hàng đợi`);
    renderKB(kb);
  };
  $("#kb-play", d).onclick = () => quickPlayDialog([kb.id]);
  $("#kb-resum", d).onclick = () => openModal(`<h2>Lập lại bản đồ “${esc(kb.name)}”?</h2>
    <p class="muted">Model tóm tắt đọc lại toàn bộ tài liệu từ phần đầu, từng chương theo từng đoạn vừa với ngữ cảnh của nó, rồi viết lại bản đồ truyện và hồ sơ nhân vật. Bộ truyện nạp bằng bản cũ cần làm việc này một lần. Phần embedding giữ nguyên.</p>
    <div class="modal-actions"><button value="cancel">Huỷ</button><button class="primary">Lập lại</button></div>`,
    async () => { await api.ResummarizeKB(kb.id); toast("Đã xếp hàng lập lại bản đồ"); renderKB(kb); });
  $("#kb-edit", d).onclick = () => editKB(kb);
  $("#kb-del", d).onclick = () => openModal(`<h2>Xoá “${esc(kb.name)}”?</h2><p class="muted">Toàn bộ tài liệu và vector của bộ truyện này sẽ bị xoá.</p>
    <div class="modal-actions"><button value="cancel">Huỷ</button><button class="primary">Xoá</button></div>`,
    async () => { await api.DeleteKnowledgeBase(kb.id); state.kbId = null; loadLibrary(); });
  const search = async () => {
    const q = $("#kb-q", d).value.trim();
    if (!q) return;
    $("#kb-hits", d).innerHTML = `<p class="muted">Đang tìm…</p>`;
    const hits = await guard(() => api.SearchKnowledge(kb.id, q));
    $("#kb-hits", d).innerHTML = hits.length ? hits.map(h =>
      `<div class="hit"><span class="chip">${esc(h.metadata.level)}${h.metadata.name ? " · " + esc(h.metadata.name) : ""}${h.metadata.pages ? " · tr." + esc(h.metadata.pages) : ""} · ${(1 - h.distance).toFixed(2)}</span>\n${esc(h.text)}</div>`).join("")
      : `<p class="muted">Không có kết quả.</p>`;
  };
  $("#kb-search", d).onclick = search;
  // A block body: returning false from an on* handler would cancel every other key.
  $("#kb-q", d).onkeydown = e => { if (e.key === "Enter" && !e.isComposing) search(); };
}

function sourceRow(s) {
  const tr = document.createElement("tr");
  tr.dataset.src = s.id;
  const cls = s.status === "done" ? "done" : s.status === "error" ? "error" : "busy";
  tr.innerHTML = `<td>${esc(s.filename)}<div class="muted small">${s.pages ? s.pages + " trang · " : ""}${esc(s.error || s.progress)}</div></td>
    <td><span class="status ${cls}">${STATUS_LABEL[s.status] ?? s.status}</span></td>
    <td style="white-space:nowrap">${BUSY.includes(s.status) ? "" : `<button data-a="re" title="Xử lý lại">↻</button>`} <button data-a="del" class="ghost danger">✕</button></td>`;
  tr.onclick = async e => {
    if (e.target.dataset.a === "re") { await guard(() => api.ReprocessSource(s.id)); }
    if (e.target.dataset.a === "del") { await guard(() => api.DeleteSource(s.id)); tr.remove(); }
  };
  return tr;
}

on("ingest:progress", s => {
  if (state.view !== "library" || state.kbId !== s.kbId) return;
  const old = $(`tr[data-src="${s.id}"]`);
  if (old) old.replaceWith(sourceRow(s));
  if (s.status === "done") api.ListKnowledgeBases().then(kbs => {
    state.kbs = kbs;
    const kb = kbs.find(k => k.id === s.kbId);
    const box = $("#kb-detail .overview");
    if (kb && box) box.textContent = kb.overview; // keep unsaved picks in the model card
  });
});

function editKB(kb = {}) {
  openModal(`<h2>${kb.id ? "Sửa" : "Tạo"} bộ truyện</h2>
    <label>Tên tác phẩm<input name="name" value="${esc(kb.name)}" required></label>
    <label>Mô tả<textarea name="description" rows="2">${esc(kb.description)}</textarea></label>
    ${kb.id ? "" : `<p class="muted small">Sau khi tạo, bạn có thể chọn model tóm tắt, vision và embedding riêng cho bộ truyện trước khi thêm tài liệu.</p>`}
    <div class="modal-actions"><button value="cancel">Huỷ</button><button class="primary">Lưu</button></div>`,
    async fd => {
      const saved = await api.SaveKnowledgeBase({ ...kb, name: fd.get("name"), description: fd.get("description") });
      state.kbId = saved.id;
      loadLibrary();
    });
}
$("#new-kb").onclick = () => editKB();

// ---------- settings ----------
const KINDS = {
  anthropic: { label: "Claude (Anthropic)", model: "claude-opus-5-5" },
  openai: { label: "OpenAI", model: "" },
  openrouter: { label: "OpenRouter", model: "" },
  nvidia: { label: "NVIDIA NIM", model: "" },
  local: { label: "Local (Ollama / LM Studio)", model: "" },
};
const BASE_URLS = { openai: "https://api.openai.com/v1", openrouter: "https://openrouter.ai/api/v1", nvidia: "https://integrate.api.nvidia.com/v1", local: "http://localhost:11434/v1", anthropic: "" };

async function loadSettings() {
  const [settings, all, ws] = await Promise.all([api.GetSettings(), api.ListProviders(), api.GetWorkerStatus()]);
  splitProviders(all);
  state.settings = settings;
  const providers = state.providers, images = state.imageProviders;
  const list = $("#provider-list");
  list.innerHTML = providers.length ? "" : `<p class="muted">Thêm ít nhất một model ngôn ngữ để bắt đầu.</p>`;
  for (const p of providers) {
    const row = document.createElement("div");
    row.className = "provider-row";
    row.innerHTML = `<div class="grow"><div>${esc(p.name)}</div><div class="muted small">${esc(KINDS[p.kind]?.label ?? p.kind)} · ${esc(p.model)}${p.vision ? " · 👁 vision" : ""}${p.hasKey ? " · 🔑" : ""}</div></div>
      <button data-a="test">Thử</button><button data-a="edit">Sửa</button>`;
    row.onclick = async e => {
      if (e.target.dataset.a === "edit") editProvider(p);
      if (e.target.dataset.a === "test") {
        e.target.disabled = true;
        try { toast("✓ " + await guard(() => api.TestProvider(p.id))); } finally { e.target.disabled = false; }
      }
    };
    list.append(row);
  }
  const ilist = $("#image-provider-list");
  ilist.innerHTML = images.length ? "" : `<p class="muted small">Chưa có provider ảnh nào — bấm “+ Thêm provider ảnh” (ComfyUI, SD WebUI, OpenAI Images, OpenRouter…).</p>`;
  for (const p of images) {
    const row = document.createElement("div");
    row.className = "provider-row";
    const t = IMAGE_TYPES[imageType(p)];
    row.innerHTML = `<div class="grow"><div>${esc(p.name)}</div><div class="muted small">${esc(t.label)}${p.model ? " · " + esc(p.model) : ""} · ${t.refs ? "nhận ảnh tham chiếu" : "không nhận ảnh tham chiếu"}${p.hasKey ? " · 🔑" : ""}</div></div>
      <button data-a="test">Vẽ thử</button><button data-a="edit">Sửa</button>`;
    row.onclick = async e => {
      if (e.target.dataset.a === "edit") editImageProvider(p);
      if (e.target.dataset.a === "test") {
        e.target.disabled = true;
        const old = e.target.textContent;
        e.target.textContent = "Đang vẽ…";
        try {
          const uri = await guard(() => api.TestImageProvider(p.id));
          openModal(`<h2>Vẽ thử: ${esc(p.name)}</h2><img src="${uri}" class="img-full" alt="">
            <div class="modal-actions"><button value="cancel" class="primary">Đóng</button></div>`, () => {});
        } finally { e.target.disabled = false; e.target.textContent = old; }
      }
    };
    ilist.append(row);
  }
  for (const sel of $$(".provider-select")) {
    sel.innerHTML = (sel.dataset.empty ? `<option value="">— Giống model chat —</option>` : "") +
      providers.map(p => `<option value="${p.id}">${esc(providerLabel(p))}</option>`).join("");
  }
  for (const sel of $$(".image-provider-select")) {
    sel.innerHTML = `<option value="">— Chưa chọn (không tạo ảnh) —</option>` +
      images.map(p => `<option value="${p.id}">${esc(p.name)} · ${esc(IMAGE_TYPES[imageType(p)].label)}</option>`).join("");
  }
  for (const el of $$("[data-setting]")) {
    const v = settings[el.dataset.setting] ?? "";
    if (el.type === "checkbox") el.checked = v === "true"; else el.value = v;
  }
  $("#draw-scene").classList.toggle("dim", !settings["provider.image"]);
  $("#worker-status").textContent = ws.dir
    ? `Worker: ${ws.running ? "đang chạy" : "đang tắt (sẽ bật khi cần)"} · ${ws.python}`
    : "⚠ Không tìm thấy thư mục worker/ — xem README để cài đặt.";
  api.LogDir().then(d => ($("#log-dir").textContent = "Thư mục: " + d));
  api.PromptsDir().then(d => ($("#prompt-dir").textContent = "Thư mục: " + d));
}

$("#open-logs").onclick = () => guard(() => api.OpenLogDir());
$("#open-prompts").onclick = () => guard(() => api.OpenPromptsDir());

// Every setting saves itself as soon as it changes (a text box when you leave it).
for (const el of $$("[data-setting]")) el.addEventListener("change", () => saveSetting(el));
async function saveSetting(el) {
  const key = el.dataset.setting;
  const value = el.type === "checkbox" ? String(el.checked) : el.value;
  await guard(() => api.SaveSettings({ [key]: value }));
  state.settings = { ...state.settings, [key]: value };
  toast("✓ Đã lưu");
  if (key === "provider.image") syncDrawButton();
}

$("#save-settings").onclick = async () => {
  const values = {};
  for (const el of $$("[data-setting]")) values[el.dataset.setting] = el.type === "checkbox" ? String(el.checked) : el.value;
  await guard(() => api.SaveSettings(values));
  state.settings = await api.GetSettings();
  syncDrawButton();
  toast("Đã lưu cài đặt");
};

function editProvider(p = { kind: "anthropic", temperature: 0, vision: false }) {
  const body = openModal(`
    <h2>${p.id ? "Sửa" : "Thêm"} model ngôn ngữ</h2>
    <label>Loại<select name="kind">${Object.entries(KINDS).map(([k, v]) => `<option value="${k}" ${k === p.kind ? "selected" : ""}>${v.label}</option>`).join("")}</select></label>
    <label>Tên hiển thị<input name="name" value="${esc(p.name)}" placeholder="Vd: Claude chính, GPT rẻ cho tóm tắt…"></label>
    <label>Base URL<input name="baseUrl" value="${esc(p.baseUrl)}"></label>
    <label>API key ${p.hasKey ? "<small>(đã lưu trong Windows Credential Manager — để trống để giữ nguyên)</small>" : ""}<input name="apiKey" type="password" autocomplete="off"></label>
    <label>Model<div class="gen-row"><input name="model" list="model-list" value="${esc(p.model)}"><button type="button" id="load-models">Tải danh sách</button></div></label>
    <datalist id="model-list"></datalist>
    <div class="row2">
      <label>Temperature (0 = mặc định)<input name="temperature" type="number" step="0.05" min="0" max="2" value="${p.temperature || 0}"></label>
      <label class="effort">Effort (Claude)<select name="effort">${["", "low", "medium", "high", "xhigh", "max"].map(e => `<option value="${e}" ${e === (p.effort || "") ? "selected" : ""}>${e || "mặc định"}</option>`).join("")}</select></label>
    </div>
    <label class="check vision"><input type="checkbox" name="vision" ${p.vision ? "checked" : ""}> Model hỗ trợ đọc ảnh (vision)</label>
    <label class="check no-think"><input type="checkbox" name="thinking" ${p.thinking === "off" ? "checked" : ""}> Tắt suy nghĩ (cho model suy luận như Qwen 3, Nemotron, DeepSeek: trả lời nhanh hơn, không tốn token suy nghĩ). Khi đọc truyện tranh, vision luôn tắt suy nghĩ.</label>
    <label>Context window (token) <small>— để trống: tự hỏi server (LM Studio, OpenRouter…), không được thì đoán ${"4096 cho local, 32768 cho API"}. Mọi prompt được cắt cho vừa con số này.</small>
      <div class="gen-row"><input name="contextWindow" type="number" min="0" step="1024" value="${p.contextWindow || ""}" placeholder="tự nhận"><button type="button" id="detect-ctx">Nhận diện</button></div></label>
    <div class="modal-actions">
      ${p.id ? `<button value="delete" class="danger ghost" style="margin-right:auto">Xoá</button>` : ""}
      <button value="cancel">Huỷ</button><button value="save" class="primary">Lưu</button>
    </div>`,
    async (fd, action) => {
      if (action === "delete") await api.DeleteProvider(p.id);
      else await api.SaveProvider(collect(fd));
      loadSettings();
    });

  const collect = fd => ({
    id: p.id || "", name: fd.get("name") || KINDS[fd.get("kind")].label, kind: fd.get("kind"), baseUrl: fd.get("baseUrl"),
    model: fd.get("model"), apiKey: fd.get("apiKey"), temperature: parseFloat(fd.get("temperature")) || 0,
    effort: fd.get("effort"), vision: fd.get("vision") === "on", thinking: fd.get("thinking") === "on" ? "off" : "",
    contextWindow: parseInt(fd.get("contextWindow")) || 0, purpose: "",
  });
  const kindSel = $("[name=kind]", body);
  const sync = first => {
    const k = kindSel.value;
    const isClaude = k === "anthropic";
    $(".effort", body).hidden = !isClaude;
    $(".vision", body).hidden = isClaude;
    $(".no-think", body).hidden = isClaude;
    if (!first || !p.id) {
      $("[name=baseUrl]", body).value = BASE_URLS[k];
      if (!$("[name=model]", body).value || !first) $("[name=model]", body).value = KINDS[k].model;
    }
  };
  kindSel.onchange = () => sync(false);
  sync(true);
  $("#detect-ctx", body).onclick = async () => {
    const n = await guard(() => api.DetectContextWindow(collect(new FormData(body))));
    if (n) { $("[name=contextWindow]", body).value = n; toast(`Model có context ${n} token`); }
    else toast("Server không cho biết — nhập tay (với LM Studio: xem Context Length khi nạp model)", true);
  };
  $("#load-models", body).onclick = async () => {
    const models = await guard(() => api.ListModels(collect(new FormData(body))));
    $("#model-list", body).innerHTML = models.map(m => `<option value="${esc(m)}">`).join("");
    toast(`${models.length} model — gõ để lọc`);
  };
}
$("#new-provider").onclick = () => editProvider();
$("#new-image-provider").onclick = () => editImageProvider();

// Ways to draw. kind/api are what the backend stores (Provider.kind, imageApi).
const IMAGE_TYPES = {
  comfyui: { label: "ComfyUI (máy bạn)", kind: "local", api: "comfyui", url: "http://127.0.0.1:8188", key: false, refs: true,
    model: "Checkpoint (điền vào {model} trong workflow)", note: "Chạy workflow của bạn: model nào, cách giữ mặt nào cũng được. Ảnh tham chiếu vào các node LoadImage có {ref1}, {ref2}…" },
  sdwebui: { label: "Stable Diffusion WebUI / Forge (máy bạn)", kind: "local", api: "sdwebui", url: "http://127.0.0.1:7860", key: false, refs: false,
    model: "Checkpoint (để trống = cái đang nạp)", note: "Chỉ vẽ theo prompt; khi cảnh nối tiếp thì vẽ đè lên ảnh trước. Không giữ được mặt nhân vật." },
  openai: { label: "OpenAI Images / API tương thích", kind: "openai", api: "images", url: "https://api.openai.com/v1", key: true, refs: true,
    model: "Model", def: "gpt-image-1", note: "gpt-image-1 nhận nhiều ảnh tham chiếu (/images/edits); dall-e chỉ vẽ theo prompt." },
  openrouter: { label: "OpenRouter (model trả về ảnh)", kind: "openrouter", api: "chat", url: "https://openrouter.ai/api/v1", key: true, refs: true,
    model: "Model", note: "Model chat có đầu ra ảnh, ví dụ Gemini image — nhận nhiều ảnh tham chiếu, mỗi ảnh kèm chú thích là ai." },
  chat: { label: "API khác kiểu OpenAI (model trả về ảnh)", kind: "openai", api: "chat", url: "", key: true, refs: true,
    model: "Model", note: "Endpoint /chat/completions có đầu ra ảnh (định dạng như OpenRouter)." },
};
function imageType(p) {
  if (p.imageApi === "comfyui" || p.imageApi === "sdwebui") return p.imageApi;
  if (p.imageApi === "images") return "openai";
  if (p.imageApi === "chat") return p.kind === "openrouter" ? "openrouter" : "chat";
  return { openai: "openai", openrouter: "openrouter", local: "sdwebui" }[p.kind] || "chat";
}

function editImageProvider(p = { imageApi: "comfyui", kind: "local" }) {
  const cur = imageType(p);
  const body = openModal(`
    <h2>${p.id ? "Sửa" : "Thêm"} provider ảnh</h2>
    <p class="muted small">Provider ảnh chỉ vẽ. Prompt và ảnh tham chiếu do model chat viết và chọn, rồi gửi sang đây.</p>
    <label>Cách vẽ<select name="type">${Object.entries(IMAGE_TYPES).map(([k, t]) => `<option value="${k}" ${k === cur ? "selected" : ""}>${t.label}</option>`).join("")}</select></label>
    <p class="muted small type-note"></p>
    <label>Tên hiển thị<input name="name" value="${esc(p.name)}" placeholder="Vd: ComfyUI máy nhà"></label>
    <label>Địa chỉ (Base URL)<input name="baseUrl" value="${esc(p.baseUrl)}"></label>
    <label class="key">API key ${p.hasKey ? "<small>(đã lưu trong Windows Credential Manager — để trống để giữ nguyên)</small>" : ""}<input name="apiKey" type="password" autocomplete="off"></label>
    <label><span class="model-label">Model</span><div class="gen-row"><input name="model" list="image-model-list" value="${esc(p.model)}"><button type="button" id="load-image-models">Tải danh sách</button></div></label>
    <datalist id="image-model-list"></datalist>
    <div class="comfy" hidden>
      <p class="muted small">Trong ComfyUI: dựng workflow → menu Workflow › <b>Export (API)</b> → dán file JSON vào đây (hoặc “Chọn file JSON…”). Đặt chỗ trống vào các ô của node:
        <code>{prompt}</code> (bắt buộc, ô text của CLIPTextEncode dương), <code>{negative}</code>, <code>{width}</code>, <code>{height}</code>, <code>{seed}</code>, <code>{model}</code> (= ô Checkpoint ở trên),
        <code>{ref1}</code>, <code>{ref2}</code>… (ô image của node LoadImage: ảnh tham chiếu theo thứ tự ảnh trước → nhân vật → trang truyện), <code>{ref_labels}</code> (mô tả ảnh nào là ai, cho model edit).</p>
      <label>Workflow có ảnh tham chiếu <small>(IP-Adapter, PuLID, Qwen-Image-Edit, Flux Kontext… — dùng khi cảnh có ảnh nhân vật)</small>
        <textarea name="workflow" rows="4" spellcheck="false" placeholder='{"3": {"class_type": "KSampler", ...}}'>${esc(p.workflow || "")}</textarea></label>
      <div class="gen-row"><button type="button" data-load="workflow">Chọn file JSON…</button></div>
      <label>Workflow không ảnh tham chiếu <small>(txt2img — dùng khi cảnh không có ảnh nào, hoặc khi bạn chỉ có workflow này)</small>
        <textarea name="workflowNoRef" rows="4" spellcheck="false">${esc(p.workflowNoRef || "")}</textarea></label>
      <div class="gen-row"><button type="button" data-load="workflowNoRef">Chọn file JSON…</button><button type="button" id="comfy-sample">Dán mẫu SDXL cơ bản</button></div>
      <input type="file" accept=".json,application/json" id="comfy-file" hidden>
    </div>
    <div class="modal-actions">
      ${p.id ? `<button value="delete" class="danger ghost" style="margin-right:auto">Xoá</button>` : ""}
      <button value="cancel">Huỷ</button><button value="save" class="primary">Lưu</button>
    </div>`,
    async (fd, action) => {
      if (action === "delete") await api.DeleteProvider(p.id);
      else await api.SaveProvider(collect(fd));
      loadSettings();
    });

  const collect = fd => {
    const t = IMAGE_TYPES[fd.get("type")];
    const comfy = t.api === "comfyui";
    return {
      id: p.id || "", purpose: "image", name: fd.get("name") || t.label, kind: t.kind, imageApi: t.api,
      baseUrl: fd.get("baseUrl"), model: fd.get("model") || "", apiKey: t.key ? fd.get("apiKey") : "",
      workflow: comfy ? (fd.get("workflow") || "").trim() : "", workflowNoRef: comfy ? (fd.get("workflowNoRef") || "").trim() : "",
    };
  };
  const typeSel = $("[name=type]", body);
  const sync = first => {
    const t = IMAGE_TYPES[typeSel.value];
    $(".type-note", body).textContent = t.note;
    $(".key", body).hidden = !t.key;
    $(".comfy", body).hidden = t.api !== "comfyui";
    $(".model-label", body).textContent = t.model;
    if (!first || !p.id) {
      $("[name=baseUrl]", body).value = t.url;
      if (!first || !$("[name=model]", body).value) $("[name=model]", body).value = t.def || "";
    }
  };
  typeSel.onchange = () => sync(false);
  sync(true);
  $("#load-image-models", body).onclick = async () => {
    const models = await guard(() => api.ListImageModels(collect(new FormData(body))));
    $("#image-model-list", body).innerHTML = models.map(m => `<option value="${esc(m)}">`).join("");
    toast(`${models.length} model — gõ để lọc`);
  };
  let loadInto = null;
  const fileIn = $("#comfy-file", body);
  $$("[data-load]", body).forEach(b => (b.onclick = () => { loadInto = b.dataset.load; fileIn.value = ""; fileIn.click(); }));
  fileIn.onchange = async () => {
    const f = fileIn.files[0];
    if (!f || !loadInto) return;
    const text = await f.text();
    try { JSON.parse(text); } catch { return toast("File không phải JSON", true); }
    $(`[name=${loadInto}]`, body).value = text;
  };
  $("#comfy-sample", body).onclick = () => {
    $("[name=workflowNoRef]", body).value = JSON.stringify(COMFY_SDXL, null, 1);
    toast("Đã dán mẫu — chọn checkpoint ở ô trên (bấm “Tải danh sách”)");
  };
}

// A basic SDXL text-to-image workflow in ComfyUI's API format, with the app's placeholders.
const COMFY_SDXL = {
  "3": { class_type: "KSampler", inputs: { seed: "{seed}", steps: 28, cfg: 6, sampler_name: "euler", scheduler: "normal", denoise: 1, model: ["4", 0], positive: ["6", 0], negative: ["7", 0], latent_image: ["5", 0] } },
  "4": { class_type: "CheckpointLoaderSimple", inputs: { ckpt_name: "{model}" } },
  "5": { class_type: "EmptyLatentImage", inputs: { width: "{width}", height: "{height}", batch_size: 1 } },
  "6": { class_type: "CLIPTextEncode", inputs: { text: "{prompt}", clip: ["4", 1] } },
  "7": { class_type: "CLIPTextEncode", inputs: { text: "{negative}", clip: ["4", 1] } },
  "8": { class_type: "VAEDecode", inputs: { samples: ["3", 0], vae: ["4", 2] } },
  "9": { class_type: "SaveImage", inputs: { filename_prefix: "immersive", images: ["8", 0] } },
};

// ---------- chats ----------
// chatSubject describes who the player talks to: a character card, or in
// scene mode the narrator of a scenario.
function chatSubject(chat) {
  if (chat?.mode === "scene") {
    const s = state.scenarios.find(x => x.id === chat.scenarioId);
    return {
      scene: true, scenario: s, name: s?.title ?? "(bối cảnh đã xoá)", speaker: "Người dẫn truyện", tag: s?.setting ?? "",
      avatar: () => avatarEl({ name: s?.title }, "avatar scene-av"),
    };
  }
  const ch = charById(chat?.characterId);
  return {
    scene: false, character: ch, name: ch?.name ?? "(nhân vật đã xoá)", speaker: ch?.name ?? "", tag: ch?.tagline || ch?.description || "",
    avatar: () => avatarEl(ch),
  };
}

async function renderChatList() {
  [state.chats, state.characters, state.scenarios] = await Promise.all([api.ListChats(), api.ListCharacters(), api.ListScenarios()]);
  const list = $("#chat-list");
  list.innerHTML = state.chats.length ? "" : `<div class="muted small" style="padding:0 12px">Chưa có cuộc trò chuyện.</div>`;
  for (const c of state.chats) {
    const sub = chatSubject(c);
    const el = document.createElement("div");
    el.className = "chat-item" + (c.id === state.chatId ? " active" : "");
    el.append(sub.avatar());
    el.insertAdjacentHTML("beforeend", `<div class="ci-name">${esc(c.title || sub.name)}</div>`);
    el.onclick = () => openChat(c.id);
    list.append(el);
  }
}

async function openChat(id) {
  state.chatId = id;
  show("chat");
  await Promise.all([renderChatList(), loadProviders(), state.kbs.length ? null : api.ListKnowledgeBases().then(k => (state.kbs = k))]);
  if (state.chatId !== id) return;
  const chat = state.chats.find(c => c.id === id);
  state.chat = chat;
  const sub = chatSubject(chat);
  $("#chat-avatar").replaceChildren(sub.avatar());
  $("#chat-name").textContent = sub.name;
  const scen = state.scenarios.find(s => s.id === chat?.scenarioId);
  const kbIds = sub.scene ? scen?.kbIds : sub.character?.kbIds;
  $("#chat-sub").textContent = (sub.scene
    ? ["Bạn vào vai " + chat.personaName]
    : [scen?.title]).concat(kbIds?.length ? kbIds.map(kbName).join(", ") : []).filter(Boolean).join(" · ");
  $("#chat-model").innerHTML = providerOptions(chat?.providerId, "chat");
  $("#input").placeholder = (sub.scene ? `${chat.personaName} làm gì, nói gì… (*hành động* trong dấu sao)` : `Nhắn tin với ${sub.name}…`)
    + " — để trống rồi gửi để truyện tự tiếp diễn";
  $("#chat-error").hidden = true;
  await renderMessages();
  setStreaming(state.streaming[id] != null);
  if (!$("#chat-drawer").hidden) renderDrawer();
  $("#input").focus();
}

$("#chat-back").onclick = () => show(state.tab);
$("#chat-model").onchange = async e => {
  await guard(() => api.SetChatProvider(state.chatId, e.target.value));
  if (state.chat) state.chat.providerId = e.target.value;
  toast("Đã đổi model: " + providerLabel(providerById(e.target.value) || defaultProvider("chat")));
};
$("#chat-info").onclick = () => {
  const d = $("#chat-drawer");
  d.hidden = !d.hidden;
  if (!d.hidden) renderDrawer();
};

function renderDrawer() {
  const chat = state.chat, sub = chatSubject(chat), ch = sub.character, scen = sub.scenario;
  const d = $("#chat-drawer");
  d.innerHTML = "";
  if (!chat) return;
  const head = document.createElement("div");
  head.className = "d-head";
  head.append(sub.avatar());
  head.insertAdjacentHTML("beforeend", `<div class="d-name">${esc(sub.name)}</div><div class="muted small">${esc(sub.scene ? "Hoá thân · AI dẫn truyện" : ch?.tagline)}</div>`);
  d.append(head);
  const history = state.chats.filter(c => sub.scene
    ? c.mode === "scene" && c.scenarioId === chat.scenarioId
    : c.mode !== "scene" && c.characterId === chat.characterId);
  const others = sub.scene ? (scen?.cast ?? []).filter(m => m.name.trim().toLowerCase() !== chat.personaName.trim().toLowerCase()) : [];
  d.insertAdjacentHTML("beforeend", `
    <div class="d-row"><button data-a="new">+ Chat mới</button>${
      sub.scene ? (scen ? `<button data-a="edit-scene">Sửa bối cảnh</button>` : "") : (ch ? `<button data-a="edit">Sửa nhân vật</button>` : "")}</div>
    <div class="d-sec"><h3>${sub.scene ? "Vai của bạn" : "Bạn trong truyện"}</h3><p><b>${esc(chat.personaName)}</b>${chat.personaDesc ? "\n" + esc(chat.personaDesc) : ""}</p></div>
    ${sub.scene && scen?.setting ? `<div class="d-sec"><h3>Bối cảnh</h3><p>${esc(scen.setting)}</p></div>` : ""}
    ${others.length ? `<div class="d-sec"><h3>Nhân vật do AI điều khiển</h3><p>${others.map(m => "• " + esc(m.name)).join("\n")}</p></div>` : ""}
    ${!sub.scene && ch?.description ? `<div class="d-sec"><h3>Mô tả</h3><p>${esc(ch.description)}</p></div>` : ""}
    <div class="d-sec"><h3>Sổ thay đổi so với nguyên tác</h3>
      <p class="muted small">Những gì câu chuyện này đã khác nguyên tác vì lựa chọn của bạn. Model tự cập nhật sau vài lượt; khi mâu thuẫn với nguyên tác, sổ này được ưu tiên.</p>
      <p class="divergence">${chat.divergence ? esc(chat.divergence) : '<span class="muted">Chưa có gì khác nguyên tác.</span>'}</p>
      <div class="d-row"><button data-a="div-edit">Sửa</button><button data-a="div-refresh">↻ Cập nhật ngay</button></div></div>
    <div class="d-sec"><h3>Lịch sử trò chuyện</h3><div class="hist">${history.map(c =>
      `<div class="hist-item ${c.id === chat.id ? "active" : ""}" data-id="${c.id}"><span>${esc(c.title || sub.name)}</span><span class="muted small">${new Date(c.updatedAt * 1000).toLocaleDateString("vi")}</span></div>`).join("")}</div></div>
    <div class="d-sec"><button data-a="delete" class="danger ghost" style="width:100%">Xoá cuộc trò chuyện này</button></div>`);
  d.onclick = e => {
    const a = e.target.closest("[data-a]")?.dataset.a;
    const item = e.target.closest(".hist-item");
    if (item) return openChat(item.dataset.id);
    if (a === "new") sub.scene ? scen && startChatDialog({ scenario: scen }) : ch && startChatDialog({ character: ch });
    if (a === "edit" && ch) editCharacter(ch, () => openChat(chat.id));
    if (a === "edit-scene" && scen) editScenario(scen);
    if (a === "delete") deleteChatDialog();
    if (a === "div-edit") editDivergence(chat);
    if (a === "div-refresh") refreshDivergence(chat, e.target.closest("button"));
  };
}

function setDivergence(chatId, text) {
  const c = state.chats.find(x => x.id === chatId);
  if (c) c.divergence = text;
  if (state.chat?.id === chatId) { state.chat.divergence = text; if (!$("#chat-drawer").hidden) renderDrawer(); }
}

function editDivergence(chat) {
  openModal(`<h2>Sổ thay đổi so với nguyên tác</h2>
    <p class="muted small">Ghi những gì đúng trong câu chuyện của bạn mà khác nguyên tác: ai còn sống, quan hệ đã đổi, đang ở đâu… Model coi sổ này là sự thật và tiếp tục cập nhật nó.</p>
    <textarea name="text" rows="14">${esc(chat.divergence || "")}</textarea>
    <div class="modal-actions"><button value="cancel">Huỷ</button><button class="primary">Lưu</button></div>`,
    async fd => {
      const text = fd.get("text").trim();
      await api.SetChatDivergence(chat.id, text);
      setDivergence(chat.id, text);
      toast("Đã lưu sổ thay đổi");
    });
}

async function refreshDivergence(chat, btn) {
  btn.disabled = true;
  btn.textContent = "Đang cập nhật…";
  try {
    const text = await guard(() => api.RefreshDivergence(chat.id));
    if (text != null) { setDivergence(chat.id, text); toast("Đã cập nhật sổ thay đổi"); }
  } finally { btn.disabled = false; btn.textContent = "↻ Cập nhật ngay"; }
}

function deleteChatDialog() {
  openModal(`<h2>Xoá cuộc trò chuyện?</h2><p class="muted">Tin nhắn và ký ức dài hạn của cuộc trò chuyện sẽ bị xoá.</p>
    <div class="modal-actions"><button value="cancel">Huỷ</button><button class="primary">Xoá</button></div>`,
    async () => { await api.DeleteChat(state.chatId); show(state.tab); });
}

async function renderMessages() {
  const id = state.chatId;
  const msgs = await api.GetMessages(id);
  if (state.chatId !== id) return;
  const box = $("#messages");
  box.innerHTML = "";
  box.append(introEl());
  const lastAssistant = msgs.length && msgs[msgs.length - 1].role === "assistant" ? msgs[msgs.length - 1].id : null;
  for (const m of msgs) box.append(msgEl(m, m.id === lastAssistant));
  if (state.streaming[id] != null) box.append(streamEl(state.streaming[id]));
  box.scrollTop = box.scrollHeight;
}

function introEl() {
  const sub = chatSubject(state.chat);
  const el = document.createElement("div");
  el.className = "chat-intro";
  el.append(sub.avatar());
  el.insertAdjacentHTML("beforeend", `<div class="ci-name">${esc(sub.name)}</div><div class="ci-tag">${esc(sub.tag)}</div>
    ${sub.scene ? `<div class="chip">Bạn vào vai ${esc(state.chat.personaName)}</div>` : ""}`);
  return el;
}

// In scene mode the narrator tags each block with its speaker ("[Narrator]",
// "[Name]"); splitScene turns a reply into {speaker, text} blocks, speaker ""
// meaning narration. Untagged text (e.g. the scenario opening) is narration.
const NARRATOR_TAG = /^(narrator|narration|người dẫn truyện|dẫn truyện|người kể chuyện)$/i;
const TIMELINE_TAG = /^(timeline|time|mốc thời gian|thời gian|dòng thời gian)$/i;
const TIMELINE = "\u0000timeline"; // speaker of a [Timeline] block
const PICTURE = "\u0000picture"; // an illustration
function splitScene(text, streaming = false) {
  const lines = text.split("\n");
  if (streaming && /^\s*\[[^\]\n]*$/.test(lines.at(-1))) lines.pop(); // half-written tag
  const blocks = [];
  let cur = { speaker: "", text: "" };
  const flush = () => {
    cur.text = cur.text.trim();
    if (!cur.text) return;
    const prev = blocks.at(-1);
    if (prev && prev.speaker === cur.speaker && cur.speaker !== PICTURE) prev.text += "\n\n" + cur.text;
    else blocks.push(cur);
  };
  for (const line of lines) {
    if (IMAGE_LINE.test(line.trim())) {
      flush();
      const speaker = cur.speaker;
      blocks.push({ speaker: PICTURE, text: line.trim() });
      cur = { speaker, text: "" };
      continue;
    }
    const m = line.match(/^\s*\*{0,2}\[([^\[\]\n]{1,40})\]\*{0,2}\s*:?\s*(.*)$/);
    if (m) {
      flush();
      const name = m[1].trim();
      cur = { speaker: NARRATOR_TAG.test(name) ? "" : TIMELINE_TAG.test(name) ? TIMELINE : name, text: m[2] };
    } else cur.text += "\n" + line;
  }
  flush();
  return blocks.length ? blocks : [{ speaker: "", text: "" }];
}

// speakerAvatar uses a matching character card's picture, else a coloured initial.
function speakerAvatar(name) {
  if (!name) return chatSubject(state.chat).avatar();
  const key = name.toLowerCase();
  const card = state.characters.find(c => c.name.trim().toLowerCase() === key);
  if (card?.avatar) return avatarEl(card);
  const el = avatarEl({ name });
  let h = 0;
  for (const ch of key) h = (h * 31 + ch.codePointAt(0)) % 360;
  el.style.background = `hsl(${h} 45% 38%)`;
  return el;
}

// sceneTurnEl renders one narrator reply: narration as plain italic prose
// (no bubble), each character's lines as their own bubble.
function sceneTurnEl(text, { streaming = false, extra = "" } = {}) {
  const turn = document.createElement("div");
  turn.className = "scene-turn" + (streaming ? " streaming" : "");
  fillSceneTurn(turn, text, streaming, extra);
  return turn;
}
function fillSceneTurn(turn, text, streaming, extra = "") {
  turn.innerHTML = "";
  const blocks = splitScene(text, streaming);
  // While streaming the turn is redrawn on every chunk: only blocks that
  // weren't there before fade in.
  const shown = Number(turn.dataset.shown || 0);
  blocks.forEach((b, i) => {
    const el = document.createElement("div");
    const last = i === blocks.length - 1;
    const body = `<div class="body${streaming && last ? " typing" : ""}">${formatMsg(b.text)}</div>`;
    if (b.speaker === PICTURE) {
      el.className = "scene-figure";
      el.innerHTML = formatMsg(b.text) + (last ? extra : "");
    } else if (b.speaker === TIMELINE) {
      el.className = "timeline-mark";
      el.innerHTML = `<span>⏱ ${esc(b.text.replace(/\s*\n\s*/g, " · "))}</span>${last ? extra : ""}`;
    } else if (b.speaker) {
      el.className = "msg assistant";
      el.append(speakerAvatar(b.speaker));
      el.insertAdjacentHTML("beforeend", `<div class="col"><div class="who">${esc(b.speaker)}</div>${body}${last ? extra : ""}</div>`);
    } else {
      el.className = "narr";
      el.innerHTML = `<span class="narr-icon" aria-hidden="true">≡</span><div class="col">${body}${last ? extra : ""}</div>`;
    }
    if (streaming && i >= shown) el.classList.add("rise");
    turn.append(el);
  });
  if (streaming) turn.dataset.shown = blocks.length;
}

function msgEl(m, canRegen = false) {
  const chat = state.chatId;
  const isUser = m.role === "user";
  const tools = `<div class="tools ${canRegen ? "always" : ""}">${canRegen ? `<button data-a="regen" title="Tạo lại">↻ Tạo lại</button>` : ""}<button data-a="edit">Sửa</button><button data-a="del">Xoá</button>`
    + `<button data-a="rollback" title="Xoá mọi tin nhắn sau tin này và chat tiếp từ đây">↩ Quay lại đây</button>`
    + `<button data-a="branch" title="Mở một cuộc trò chuyện mới tiếp nối từ tin này, giữ nguyên cuộc hiện tại">⑂ Rẽ nhánh</button>`
    + (state.settings?.["provider.image"] ? `<button data-a="draw" title="Vẽ phân cảnh của tin nhắn này: model chat viết prompt, app gắn ảnh tham chiếu nhân vật, provider ảnh vẽ">🖼 Tạo ảnh</button>` : "") + `</div>`;
  let el;
  if (!isUser && state.chat?.mode === "scene") {
    el = sceneTurnEl(m.content, { extra: tools });
    if (m.summarized) (el.querySelector(".who") || el.querySelector(".body"))?.insertAdjacentHTML("beforeend", `<span class="badge">đã vào ký ức</span>`);
  } else {
    el = document.createElement("div");
    el.className = "msg " + m.role;
    const who = esc(isUser ? state.chat?.personaName || "Bạn" : chatSubject(state.chat).speaker);
    el.innerHTML = `<div class="col"><div class="who">${who}${m.summarized ? `<span class="badge">đã vào ký ức</span>` : ""}</div>
      <div class="body">${formatMsg(m.content)}</div>${tools}</div>`;
    if (!isUser) el.prepend(chatSubject(state.chat).avatar());
  }
  el.querySelector(".tools").onclick = async e => {
    const a = e.target.dataset.a;
    if (a === "regen") regenerate();
    if (a === "rollback") rollbackDialog(chat, m.id);
    if (a === "branch") branchFrom(chat, m.id);
    if (a === "draw") drawMessage(chat, m.id);
    if (a === "del") { await guard(() => api.DeleteMessage(m.id)); el.remove(); }
    if (a === "edit") {
      openModal(`<h2>Sửa tin nhắn</h2><textarea name="c" rows="8">${esc(m.content)}</textarea>
        <div class="modal-actions"><button value="cancel">Huỷ</button><button class="primary">Lưu</button></div>`,
        async fd => { await api.EditMessage(m.id, fd.get("c")); if (state.chatId === chat) renderMessages(); });
    }
  };
  el.dataset.msgId = m.id;
  if (drawingMsgs[m.id]) el.querySelector(".tools")?.insertAdjacentHTML("beforebegin", drawingHTML(m.id));
  if (drawErrors[m.id]) el.querySelector(".tools")?.insertAdjacentHTML("beforebegin", drawErrorHTML(m.id));
  return el;
}

// Pictures being drawn from the Tạo ảnh button: message id → progress text,
// shown under the message (kept across re-renders).
const drawingMsgs = {};
const DRAW_STAGE = {
  writing: "🖼 Model đang đọc phân cảnh, tra truyện và viết prompt…",
  drawing: "🖼 Đang vẽ (đã gắn ảnh tham chiếu nhân vật)…",
};
// Why a drawing failed, kept under its message until dismissed.
const drawErrors = {};
function drawErrorHTML(id) {
  return `<div class="draw-error" data-draw-err="${id}"><span>⚠ Không tạo được ảnh: ${esc(drawErrors[id])}</span>
    <span class="draw-error-actions"><button type="button" data-a="log">Mở model log</button><button type="button" data-a="x" title="Ẩn">✕</button></span></div>`;
}
function setDrawError(id, error) {
  if (error) drawErrors[id] = error; else delete drawErrors[id];
  $(`[data-draw-err="${id}"]`)?.remove();
  if (error) $(`[data-msg-id="${id}"] .tools`)?.insertAdjacentHTML("beforebegin", drawErrorHTML(id));
}
document.addEventListener("click", e => {
  const box = e.target.closest?.(".draw-error");
  if (!box) return;
  if (e.target.dataset.a === "log") guard(() => api.OpenLogDir());
  if (e.target.dataset.a === "x") setDrawError(box.dataset.drawErr, null);
});

async function drawMessage(chatId, id) {
  if (!state.settings?.["provider.image"]) return toast("Chưa chọn provider ảnh — vào Cài đặt › Tạo ảnh minh hoạ › “Provider ảnh đang dùng”", true);
  if (drawingMsgs[id]) return toast("Tin nhắn này đang được vẽ");
  setDrawError(id, null);
  await guard(() => api.IllustrateMessage(chatId, id));
  setDrawing(id, DRAW_STAGE.writing);
}
// The composer's 🖼 draws the newest message of the chat.
$("#draw-scene").onclick = () => {
  const last = $$("#messages [data-msg-id]").at(-1);
  if (!last) return toast("Chưa có tin nhắn nào để vẽ", true);
  drawMessage(state.chatId, Number(last.dataset.msgId));
};
function syncDrawButton() {
  const on = !!state.settings?.["provider.image"];
  const b = $("#draw-scene");
  b.classList.toggle("dim", !on);
  b.title = on ? "Tạo ảnh phân cảnh hiện tại (tin nhắn mới nhất). Muốn vẽ tin khác: rê chuột vào tin đó › 🖼 Tạo ảnh"
    : "Chưa chọn provider ảnh — vào Cài đặt › Tạo ảnh minh hoạ";
  if (state.view === "chat" && state.chatId) renderMessages();
}
function drawingHTML(id) { return `<div class="drawing thinking" data-draw-msg="${id}">${esc(drawingMsgs[id])}</div>`; }
function setDrawing(id, text) {
  if (text) drawingMsgs[id] = text; else delete drawingMsgs[id];
  const old = $(`[data-draw-msg="${id}"]`);
  if (!text) return old?.remove();
  if (old) return (old.textContent = text);
  const tools = $(`[data-msg-id="${id}"] .tools`);
  tools?.insertAdjacentHTML("beforebegin", drawingHTML(id));
}

// rollbackDialog returns the chat to message msgId, or branches from it instead.
function rollbackDialog(chatId, msgId) {
  if (state.streaming[chatId] != null) return toast("Đợi trả lời xong (hoặc bấm dừng) rồi hãy quay lại", true);
  openModal(`<h2>Quay lại tin nhắn này?</h2>
    <p class="muted">Mọi tin nhắn sau tin này sẽ bị xoá và câu chuyện tiếp tục từ đây. Sổ thay đổi trở về như lúc đó.</p>
    <p class="muted small">Muốn giữ lại diễn biến hiện tại thì chọn “Rẽ nhánh”: một cuộc trò chuyện mới bắt đầu từ tin này, cuộc cũ giữ nguyên.</p>
    <div class="modal-actions"><button value="cancel">Huỷ</button><button value="branch">⑂ Rẽ nhánh</button><button value="rollback" class="primary">↩ Quay lại</button></div>`,
    async (_, action) => {
      if (action === "branch") return branchFrom(chatId, msgId);
      await api.RollBackChat(chatId, msgId);
      if (state.chatId === chatId) { await openChat(chatId); toast("Đã quay lại — chat tiếp từ đây"); }
    });
}

async function branchFrom(chatId, msgId) {
  if (state.streaming[chatId] != null) return toast("Đợi trả lời xong (hoặc bấm dừng) rồi hãy rẽ nhánh", true);
  const c = await guard(() => api.BranchChat(chatId, msgId));
  if (!c) return;
  await openChat(c.id);
  toast("Đã rẽ nhánh — cuộc trò chuyện cũ vẫn giữ nguyên trong lịch sử");
}

function streamEl(text) {
  if (state.chat?.mode === "scene") return sceneTurnEl(text, { streaming: true });
  const el = document.createElement("div");
  el.className = "msg assistant streaming";
  const sub = chatSubject(state.chat);
  el.append(sub.avatar());
  el.insertAdjacentHTML("beforeend", `<div class="col"><div class="who">${esc(sub.speaker)}</div><div class="body typing">${formatMsg(text)}</div></div>`);
  return el;
}

function setStreaming(on) {
  $("#stop").hidden = !on;
  $("#send").disabled = on;
  $$(".msg [data-a=regen]").forEach(b => (b.disabled = on));
}

async function regenerate() {
  $("#chat-error").hidden = true;
  await guard(() => api.Regenerate(state.chatId));
  renderMessages();
}

on("chat:start", ({ chatId }) => {
  state.streaming[chatId] = "";
  if (chatId === state.chatId && state.view === "chat") { $("#messages").append(streamEl("")); setStreaming(true); scrollDown(); }
});
on("chat:delta", ({ chatId, delta }) => {
  state.streaming[chatId] = (state.streaming[chatId] ?? "") + delta;
  if (chatId !== state.chatId) return;
  $$(".streaming .drawing").forEach(d => d.remove());
  const turn = $(".scene-turn.streaming");
  const body = $(".msg.streaming .body");
  if (turn) fillSceneTurn(turn, state.streaming[chatId], true);
  else if (body) body.innerHTML = formatMsg(state.streaming[chatId]);
  scrollDown();
});
// Reasoning models think before answering: say so until the first answer text arrives.
on("chat:thinking", ({ chatId }) => {
  if (chatId !== state.chatId || state.streaming[chatId]) return;
  const body = $$(".streaming .body").at(-1);
  if (body && !body.querySelector(".thinking")) body.innerHTML = `<span class="thinking">Đang suy nghĩ…</span>`;
});
// The model is looking the source up before it answers.
const LOOKUP = { search_story: "tìm trong truyện", read_chapter: "đọc chương", get_character: "xem hồ sơ" };
on("chat:lookup", ({ chatId, tool, args }) => {
  if (chatId !== state.chatId || state.streaming[chatId]) return;
  const body = $$(".streaming .body").at(-1);
  if (!body) return;
  let what = "";
  try { const a = JSON.parse(args || "{}"); what = a.query || a.name || (a.chapter ? "chương " + a.chapter : ""); } catch { }
  body.innerHTML = `<span class="thinking">Đang ${LOOKUP[tool] ?? "tra cứu"}${what ? ": " + esc(String(what)) : ""}…</span>`;
});
on("chat:divergence", ({ chatId, divergence }) => setDivergence(chatId, divergence));
// An illustration is being drawn (during a reply, or from the Tạo ảnh button).
on("chat:image", ({ chatId, status, error }) => {
  if (chatId !== state.chatId) return;
  if (status === "error") return toast("Không vẽ được ảnh: " + error, true);
  if (status !== "drawing" || state.streaming[chatId] == null) return;
  let body = $$(".streaming .body").at(-1);
  if (!body || state.streaming[chatId]) {
    $$(".streaming .drawing").forEach(d => d.remove());
    $(".scene-turn.streaming, .msg.streaming .col")?.insertAdjacentHTML("beforeend", `<div class="drawing thinking">🖼 Đang vẽ minh hoạ…</div>`);
  } else body.innerHTML = `<span class="thinking">🖼 Đang vẽ minh hoạ…</span>`;
  scrollDown();
});
on("chat:illustrating", ({ msgId, stage }) => setDrawing(msgId, DRAW_STAGE[stage]));
on("chat:illustrated", ({ chatId, msgId, refs }) => {
  setDrawing(msgId, null);
  if (chatId === state.chatId && state.streaming[chatId] == null) renderMessages();
  toast(refs?.length ? "Đã vẽ xong — ảnh tham chiếu: " + refs.join("; ") : "Đã vẽ xong (không tìm được ảnh tham chiếu nào, vẽ theo mô tả)");
});
on("chat:illustrate-error", ({ msgId, error }) => {
  setDrawing(msgId, null);
  setDrawError(msgId, error);
  toast("Không tạo được ảnh — xem lý do dưới tin nhắn", true);
});
on("chat:done", ({ chatId }) => {
  delete state.streaming[chatId];
  if (chatId === state.chatId) { setStreaming(false); renderMessages(); }
  if (!$("#recent").hidden) renderChatList();
});
on("chat:error", ({ chatId, error }) => {
  delete state.streaming[chatId];
  if (chatId !== state.chatId) return toast(error, true);
  setStreaming(false);
  $(".msg.streaming, .scene-turn.streaming")?.remove();
  const box = $("#chat-error");
  box.textContent = "⚠ " + error;
  box.hidden = false;
});

function scrollDown() {
  const box = $("#messages");
  if (box.scrollHeight - box.scrollTop - box.clientHeight < 200) box.scrollTop = box.scrollHeight;
}

$("#composer").onsubmit = async e => {
  e.preventDefault();
  const input = $("#input");
  const text = input.value.trim();
  if (state.streaming[state.chatId] != null) return;
  if (!text) {
    // Nothing typed: the story carries on by itself.
    $("#chat-error").hidden = true;
    $$(".tools.always").forEach(t => { t.classList.remove("always"); t.querySelector("[data-a=regen]")?.remove(); });
    await guard(() => api.ContinueChat(state.chatId));
    return;
  }
  input.value = "";
  autoGrow();
  $("#chat-error").hidden = true;
  const m = await guard(() => api.SendMessage(state.chatId, text));
  $$(".msg .tools.always").forEach(t => { t.classList.remove("always"); t.querySelector("[data-a=regen]")?.remove(); });
  const box = $("#messages"), streaming = $(".msg.streaming", box);
  streaming ? streaming.before(msgEl(m)) : box.append(msgEl(m)); // the reply may already be streaming
  box.scrollTop = box.scrollHeight;
};
$("#input").onkeydown = e => {
  if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); $("#composer").requestSubmit(); }
};
const autoGrow = () => { const t = $("#input"); t.style.height = "auto"; t.style.height = Math.min(t.scrollHeight, 200) + "px"; };
$("#input").oninput = autoGrow;
$("#stop").onclick = () => api.StopGeneration(state.chatId);

// ---------- boot ----------
window.addEventListener("load", async () => {
  await loadProviders();
  show(state.providers.length ? "characters" : "settings");
  if (!state.providers.length) toast("Chào mừng! Hãy thêm một provider (Claude, OpenAI, OpenRouter, NVIDIA hoặc Local) để bắt đầu.");
});
