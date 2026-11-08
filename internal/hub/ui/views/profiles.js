// /profiles (admins): code-session profiles, chat models and prices, each edited as
// JSON (DESIGN §5, §17.1, §28), and the two settings. The hub validates; its error
// message shows under the editor. Every entry but the built-in subscription profile
// is editable: saving a hub.toml entry stores an override in the hub's database,
// which "Revert to hub.toml" drops again (hub.toml itself is never written).
import { html, useState, useEffect } from "../lib.js";
import { api, get } from "../api.js";
import { toast } from "../components/toast.js";
import { Page } from "./page.js";

const help = {
  profile: html`Router model names (upstream names, <code>main</code>, alias targets) use [A-Za-z0-9._-], no <code>/</code>.
    Optional per upstream: <code>context_window</code>, <code>max_output</code> (tokens; used for the session's main model).
    Keys live in the secrets file and are referenced by <code>key_ref</code>.
    A changed upstream <code>model</code> reaches running sessions on their next request;
    alias changes need a Resume (aliases are set when claude starts).`,
  model: html`Names use [A-Za-z0-9._-]. Optional: <code>label</code>, <code>auth_header</code> (bearer or x-api-key),
    <code>context_window</code> (default 131072), <code>max_output</code> (default 32000), <code>vision</code>,
    <code>documents</code> (native PDF blocks; default = vision), <code>default</code>.
    Sessions keep the model name, so changing <code>model</code> here moves them to a new model id on the next turn.`,
  price: html`Fields as in <code>[[prices]]</code>: <code>model</code> (the upstream's model id; a trailing <code>*</code> matches a prefix),
    <code>input</code>, <code>output</code>, <code>cache_read</code>, <code>cache_write</code>, per million tokens.`,
};

const endpoints = {
  profile: { save: "/api/admin/profiles", revert: (/** @type {string} */ n) => `/api/admin/profiles/${encodeURIComponent(n)}/override` },
  model: { save: "/api/admin/chat-models", revert: (/** @type {string} */ n) => `/api/admin/chat-models/${encodeURIComponent(n)}/override` },
  price: {
    save: "/api/admin/prices",
    revert: (/** @type {string} */ n) => `/api/admin/prices/override?model=${encodeURIComponent(n)}`,
    del: (/** @type {string} */ n) => `/api/admin/prices?model=${encodeURIComponent(n)}`,
  },
};
const nouns = { profile: "profile", model: "chat model", price: "price" };
const titles = { profile: "Profile JSON (DESIGN §5)", model: "Chat model JSON (DESIGN §17.1)", price: "Price JSON (DESIGN §17.6)" };

function sourceNote(x) {
  if (x.source === "builtin") return "built in";
  if (x.overrides_config) return x.differs_from_config ? "overrides hub.toml (differs)" : "overrides hub.toml";
  if (x.source === "config") return "hub.toml";
  return "";
}

/** Key of an upstream model list: base_url and key_ref. */
const listKey = (/** @type {string} */ base, /** @type {string} */ ref) => base + "\n" + (ref || "");

/** The upstream's entry for a model id (ids compared without case), or null. */
function findModel(list, id) {
  const l = (id || "").toLowerCase();
  return list.find((m) => (m.id || "").toLowerCase() === l) || null;
}

/** A retire date in the browser's local time, e.g. 2026-11-06 00:00 in JST. */
function localDate(/** @type {string} */ s) {
  const t = new Date(s);
  if (isNaN(t.getTime())) return s;
  const p = (/** @type {number} */ n) => String(n).padStart(2, "0");
  return `${t.getFullYear()}-${p(t.getMonth() + 1)}-${p(t.getDate())} ${p(t.getHours())}:${p(t.getMinutes())}`;
}

/** Pills for one model id: retiring, not listed (DESIGN §28.6), no price (§28.2). */
function ModelNotes({ lists, base, keyRef, model, priced }) {
  const list = lists[listKey(base, keyRef)];
  const e = list ? findModel(list, model) : null;
  return html`${list && e && e.deprecated_at && html` <span class="pill bad">retires ${localDate(e.deprecated_at)}</span>`}
    ${list && !e && html` <span class="pill warn">not in the upstream's model list</span>`}
    ${!priced && html` <span class="pill warn">no price: cost recorded as 0</span>`}`;
}

function Editor({ kind, item, template, keys, onSaved, onClose }) {
  const isNew = !item;
  const readOnly = !!item && !item.editable;
  const [text, setText] = useState(item ? item.json : template);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => { setText(item ? item.json : template); setErr(""); }, [item && item.name, item && item.json, kind]);
  const run = async (/** @type {() => Promise<any>} */ f, /** @type {string} */ done) => {
    setBusy(true);
    setErr("");
    try {
      const r = await f();
      toast(done + " " + r.name, "info");
      onSaved(done === "deleted" ? "" : r.name);
    } catch (x) {
      setErr(x.message);
    } finally {
      setBusy(false);
    }
  };
  const ep = endpoints[kind];
  const save = (e) => {
    e.preventDefault();
    run(() => api("POST", ep.save, { json: text, original: item ? item.name : "" }), "saved");
  };
  const revert = () => {
    if (confirm("Drop the changes made here and use the hub.toml version of " + item.name + "?")) run(() => api("DELETE", ep.revert(item.name)), "reverted");
  };
  const del = () => {
    if (confirm("Delete " + item.name + "?")) run(() => api("DELETE", ep.del(item.name)), "deleted");
  };
  const noun = nouns[kind];
  const canDelete = kind === "price" && item && item.source === "ui" && !item.overrides_config;
  return html`<section class="editor card">
    <div class="card-head">
      <h2>${isNew ? "New " + noun : noun[0].toUpperCase() + noun.slice(1) + " " + item.name}</h2>
      <button type="button" class="link" onClick=${onClose}>Close</button>
    </div>
    ${readOnly && html`<p class="notice warn">Built in.</p>`}
    ${item && item.source === "config" && html`<p class="notice">From hub.toml. Saving stores an override in the hub's
      database; hub.toml is not changed, and a later change there does not replace the override.</p>`}
    ${item && item.overrides_config && html`<p class="notice warn">Overrides hub.toml${item.differs_from_config ? " (differs from it)" : " (same as it)"}.
      Changes to this entry in hub.toml are ignored until you revert.</p>`}
    <form class="stack" onSubmit=${save}>
      <label>${titles[kind]}
        <textarea class="code" rows=${kind === "price" ? 8 : 16} spellcheck="false" readonly=${readOnly} value=${text}
          onInput=${(e) => setText(e.currentTarget.value)}></textarea>
      </label>
      ${err && html`<p class="notice bad" role="alert">${err}</p>`}
      <p class="muted small">${help[kind]}${kind !== "price" && html` Known key_refs: ${keys.length ? keys.map((k) => html`<code key=${k}>${k}</code> `) : "none"}`}</p>
      ${!readOnly && html`<div class="actions">
        <button type="submit" disabled=${busy}>Save</button>
        ${item && item.overrides_config && html` <button type="button" class="secondary" disabled=${busy} onClick=${revert}>Revert to hub.toml</button>`}
        ${canDelete && html` <button type="button" class="secondary" disabled=${busy} onClick=${del}>Delete</button>`}
      </div>`}
    </form>
  </section>`;
}

function Settings({ d, onSaved }) {
  const names = d.chat_models.map((m) => m.name);
  const labels = { "auto.classifier": "Auto mode classifier (DESIGN §18.4)", "concierge.model": "Concierge model (DESIGN §20)" };
  const empty = { "auto.classifier": "none (approvals no rule decides go to you)", "concierge.model": "the chat default" };
  const call = async (/** @type {string} */ method, /** @type {string} */ path, body, /** @type {string} */ done) => {
    try {
      await api(method, path, body);
      toast(done, "info");
      onSaved();
    } catch (x) {
      toast(x.message);
    }
  };
  return html`<ul class="list">${d.settings.map((s) => html`<li key=${s.key}>
    <label>${labels[s.key] || s.key}
      <select value=${s.value} onChange=${(e) => call("POST", "/api/admin/settings", { key: s.key, value: e.currentTarget.value }, "saved " + s.key)}>
        <option value="">${empty[s.key] || "none"}</option>
        ${names.map((n) => html`<option key=${n} value=${n}>${n}</option>`)}
        ${s.value && !names.includes(s.value) && html`<option value=${s.value}>${s.value} (not a chat model)</option>`}
      </select>
    </label>
    <div class="muted small">hub.toml: ${s.config_value || "not set"}
      ${s.overrides_config && html` · <span class="pill accent">overrides hub.toml</span>
        <button type="button" class="link" onClick=${() => call("DELETE", `/api/admin/settings/${encodeURIComponent(s.key)}/override`, undefined, "reverted " + s.key)}>Revert to hub.toml</button>`}</div>
  </li>`)}</ul>`;
}

export function ProfilesView({ app, search }) {
  const [d, setD] = useState(null);
  const [lists, setLists] = useState(/** @type {Record<string, any[]>} */ ({}));
  const edit = new URLSearchParams(search).get("edit") || ""; // "profile:<name>", "model:new", "price:<model>", …
  const load = () => get("/api/admin/profiles").then(setD, (e) => toast(e.message));
  useEffect(() => { load(); }, []);
  // The upstreams' model lists (DESIGN §28.6), once per base_url and key_ref; a list
  // that cannot be loaded shows nothing.
  useEffect(() => {
    if (!d) return;
    const pairs = new Map();
    for (const p of d.profiles) for (const u of p.upstreams || []) pairs.set(listKey(u.base_url, u.key_ref), u);
    for (const m of d.chat_models) pairs.set(listKey(m.base_url, m.key_ref), m);
    for (const [k, u] of pairs) {
      if (k in lists) continue;
      get(`/api/admin/upstream-models?base_url=${encodeURIComponent(u.base_url)}&key_ref=${encodeURIComponent(u.key_ref || "")}`)
        .then((r) => setLists((o) => ({ ...o, [k]: r.models || [] })), () => {});
    }
  }, [d]);
  if (!d) return html`<${Page} app=${app} title="Profiles & chat models" wide><div class="muted">Loading…</div><//>`;
  const [ek, ...rest] = edit.split(":");
  const en = rest.join(":");
  const open = (/** @type {string} */ v) => app.navigate("/profiles" + (v ? "?edit=" + encodeURIComponent(v) : ""));
  const prices = d.prices.map((p) => ({ ...p, name: p.model }));
  const kind = ek === "profile" || ek === "model" || ek === "price" ? ek : "";
  const list = kind === "profile" ? d.profiles : kind === "model" ? d.chat_models : prices;
  const item = kind && en !== "new" ? list.find((x) => x.name === en) : null;
  const templates = { profile: d.profile_template, model: d.chat_model_template, price: d.price_template };
  const editor = kind && (en === "new" || item) && html`<${Editor} kind=${kind} item=${item} keys=${d.keys}
    template=${templates[kind]}
    onSaved=${(name) => { load(); open(name ? kind + ":" + name : ""); }} onClose=${() => open("")} />`;

  // Drift (DESIGN §28.5): overrides, and models the upstreams retire.
  const overrides = [...d.profiles, ...d.chat_models, ...d.prices, ...d.settings].filter((x) => x.overrides_config).length;
  const ids = [];
  for (const p of d.profiles) for (const u of p.upstreams || []) ids.push([u.base_url, u.key_ref, u.model]);
  for (const m of d.chat_models) ids.push([m.base_url, m.key_ref, m.model]);
  const retiring = new Set(ids.filter(([b, k, id]) => {
    const l = lists[listKey(b, k)];
    const e = l && findModel(l, id);
    return e && e.deprecated_at;
  }).map(([, , id]) => id.toLowerCase())).size;
  const drift = [overrides > 0 && `${overrides} ${overrides === 1 ? "entry overrides" : "entries override"} hub.toml`,
    retiring > 0 && `${retiring} ${retiring === 1 ? "model" : "models"} retiring`].filter(Boolean);

  const row = (/** @type {string} */ k, x, sub) => html`<li key=${x.name} class=${edit === k + ":" + x.name ? "active" : ""}>
    <a href=${"/profiles?edit=" + encodeURIComponent(k + ":" + x.name)}><strong>${x.name}</strong></a>
    ${sub.pill && html` <span class="pill">${sub.pill}</span>`}
    ${sub.flags.map((f) => html` <span class="pill accent">${f}</span>`)}
    ${sourceNote(x) && html` <span class="muted small">· ${sourceNote(x)}</span>`}
    <div class="muted small">${sub.line}</div>
    ${sub.extra}
  </li>`;
  const upstreamLines = (p) => (p.upstreams || []).map((u) => html`<div class="muted small" key=${u.name}>
    ${u.name}: <code>${u.model}</code><${ModelNotes} lists=${lists} base=${u.base_url} keyRef=${u.key_ref} model=${u.model} priced=${u.priced} /></div>`);
  const cur = d.currency || "";
  return html`<${Page} app=${app} title="Profiles & chat models" wide>
    ${drift.length > 0 && html`<p class="notice warn">${drift.join(" · ")}. hub.toml is not the whole truth: see the entries marked below.</p>`}
    ${editor}
    <div class="section-row"><h3 class="section-h">Profiles (code sessions)</h3>
      <button type="button" class="secondary" onClick=${() => open("profile:new")}>New profile</button></div>
    <ul class="list">${d.profiles.map((p) => row("profile", p, { pill: p.kind, flags: [], line: p.summary, extra: upstreamLines(p) }))}</ul>
    <div class="section-row"><h3 class="section-h">Chat models</h3>
      <button type="button" class="secondary" onClick=${() => open("model:new")}>New chat model</button></div>
    ${!d.chat_enabled && html`<p class="notice warn">Chat is disabled in hub.toml (<code>[chat] enabled = false</code>).</p>`}
    <ul class="list">${d.chat_models.length === 0 ? html`<li class="muted">none</li>` :
      d.chat_models.map((m) => row("model", m, { pill: m.label, flags: [m.default && "default", m.vision && "vision"].filter(Boolean),
        line: html`<code>${m.model}</code><${ModelNotes} lists=${lists} base=${m.base_url} keyRef=${m.key_ref} model=${m.model} priced=${m.priced} />` }))}</ul>
    <div class="section-row"><h3 class="section-h">Prices${cur && ` (${cur} per million tokens)`}</h3>
      <button type="button" class="secondary" onClick=${() => open("price:new")}>Add price</button></div>
    <ul class="list">${prices.length === 0 ? html`<li class="muted">none: every cost is recorded as 0</li>` :
      prices.map((p) => row("price", p, { flags: [],
        line: `input ${p.input} · output ${p.output} · cache read ${p.cache_read} · cache write ${p.cache_write}` }))}</ul>
    <h3 class="section-h">Settings</h3>
    <${Settings} d=${d} onSaved=${load} />
  <//>`;
}
