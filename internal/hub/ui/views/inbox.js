// /inbox (DESIGN §23.8): collected items, digests, tasks and (admins) accounts.
// Item text is third-party content: it is always rendered as text, never as HTML.
// Only the digest, which the hub renders and sanitizes like chat answers, is HTML.
import { html, useState, useEffect, useRef, useCallback } from "../lib.js";
import { api, get } from "../api.js";
import { toast } from "../components/toast.js";
import { Page } from "./page.js";
import { cx, fmtMoney, fmtTime } from "../util.js";

const tabs = [["items", "Items"], ["digests", "Digests"], ["tasks", "Tasks"], ["accounts", "Accounts"]];

/** Compact time: HH:MM today, MM-DD this year, else the date. */
function shortTime(/** @type {string} */ iso) {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const now = new Date();
  const p = (/** @type {number} */ n) => String(n).padStart(2, "0");
  if (d.toDateString() === now.toDateString()) return `${p(d.getHours())}:${p(d.getMinutes())}`;
  if (d.getFullYear() === now.getFullYear()) return `${p(d.getMonth() + 1)}-${p(d.getDate())}`;
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

function firstLine(/** @type {string} */ s) {
  return (s || "").trim().split("\n")[0];
}

function itemHead(it) {
  return it.title || firstLine(it.text) || "(no text)";
}

/** An item opened in full: text as text, the link out, the state buttons. */
function ItemBody({ it, onState }) {
  const [full, setFull] = useState(it);
  useEffect(() => {
    setFull(it);
    if (it.text_truncated) get(`/api/inbox/items/${it.id}`).then(setFull, (e) => toast(e.message));
  }, [it.id]);
  const body = (full.text || "").trim();
  return html`<div class="item-body">
    ${full.title && body && body !== full.title && html`<div class="item-title">${full.title}</div>`}
    ${body && html`<div class="item-text">${body}</div>`}
    <div class="item-meta muted small">${fmtTime(full.ts)} · ${full.account}${full.kind ? " · " + full.kind : ""}
      ${full.author ? " · @" + full.author : ""} · #${full.id}</div>
    <div class="buttons">
      ${full.url && html`<a class="button secondary" href=${full.url} target="_blank" rel="noopener noreferrer nofollow">Open ↗</a>`}
      ${onState && full.state !== "read" && html`<button type="button" class="secondary" onClick=${() => onState(full, "read")}>Read</button>`}
      ${onState && full.state !== "archived" && html`<button type="button" class="secondary" onClick=${() => onState(full, "archived")}>Archive</button>`}
      ${onState && full.state !== "new" && html`<button type="button" class="secondary" onClick=${() => onState(full, "new")}>Mark new</button>`}
    </div>
  </div>`;
}

function ItemRow({ it, open, onToggle, onState }) {
  return html`<li class=${cx("inbox-item", it.state !== "new" && "seen", open && "open")}>
    <button type="button" class="item-head" aria-expanded=${open} onClick=${() => onToggle(it.id)}>
      <span class="item-line1">
        <span class="item-time">${shortTime(it.ts)}</span>
        <span class="pill item-acct">${it.account}</span>
        ${it.author && html`<span class="item-author">${it.author}</span>`}
      </span>
      <span class="item-line2">${itemHead(it)}</span>
    </button>
    ${open && html`<${ItemBody} it=${it} onState=${onState} />`}
  </li>`;
}

function ItemsTab({ app, accounts }) {
  const [account, setAccount] = useState("");
  const [state, setState] = useState("new");
  const [q, setQ] = useState("");
  const [query, setQuery] = useState("");
  const [items, setItems] = useState(null);
  const [more, setMore] = useState(false);
  const [open, setOpen] = useState(0);
  const timer = useRef(0);
  const params = (/** @type {number} */ before) => {
    const p = new URLSearchParams({ state, limit: "50" });
    if (account) p.set("account", account);
    if (query) p.set("q", query);
    if (before) p.set("before", String(before));
    return p;
  };
  const load = useCallback(() => {
    get("/api/inbox/items?" + params(0)).then((r) => { setItems(r.items); setMore(r.more); }, (e) => toast(e.message));
  }, [account, state, query]);
  useEffect(() => { load(); }, [load]);
  const loadMore = () => {
    const last = items[items.length - 1];
    get("/api/inbox/items?" + params(last.id)).then((r) => { setItems([...items, ...r.items]); setMore(r.more); }, (e) => toast(e.message));
  };
  const onSearch = (/** @type {string} */ v) => {
    setQ(v);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setQuery(v.trim()), 300);
  };
  const onState = async (it, st) => {
    try {
      await api("PATCH", `/api/inbox/items/${it.id}`, { state: st });
      setItems((xs) => (state !== "all" && st !== state ? xs.filter((x) => x.id !== it.id) : xs.map((x) => (x.id === it.id ? { ...x, state: st } : x))));
      if (state !== "all" && st !== state) setOpen(0);
    } catch (e) { toast(e.message); }
  };
  const markAll = async () => {
    try {
      const r = await api("POST", "/api/inbox/items/state", { all_new: true, account });
      toast(`${r.changed} marked read`, "info");
      load();
    } catch (e) { toast(e.message); }
  };
  const mine = accounts.filter((a) => a.owner_sub === app.me.sub);
  return html`<div class="inbox-tab">
    <div class="inbox-filters">
      <label class="search inbox-search"><input type="search" placeholder="Search items" value=${q}
        onInput=${(e) => onSearch(e.currentTarget.value)} aria-label="Search items" /></label>
      <select aria-label="Account" value=${account} onChange=${(e) => setAccount(e.currentTarget.value)}>
        <option value="">All accounts</option>
        ${mine.map((a) => html`<option key=${a.id} value=${a.id}>${a.name}${a.new_items ? ` (${a.new_items})` : ""}</option>`)}
      </select>
      <select aria-label="State" value=${state} onChange=${(e) => setState(e.currentTarget.value)}>
        <option value="new">New</option><option value="read">Read</option><option value="archived">Archived</option><option value="all">All</option>
      </select>
      ${state === "new" && items && items.length > 0 && html`<button type="button" class="secondary" onClick=${markAll}>Mark all read</button>`}
    </div>
    ${!items ? html`<div class="muted">Loading…</div>`
      : items.length === 0 ? html`<div class="muted empty-inbox">${query ? "No match." : state === "new" ? "Nothing new." : "No items."}
          ${mine.length === 0 && html`<div class="small">You have no inbox accounts yet${app.me.is_admin ? "; add one under Accounts." : "; an admin adds them."}</div>`}</div>`
      : html`<ul class="list inbox-list">${items.map((it) => html`<${ItemRow} key=${it.id} it=${it} open=${open === it.id}
          onToggle=${(id) => setOpen(open === id ? 0 : id)} onState=${onState} />`)}</ul>`}
    ${more && html`<div class="load-earlier"><button type="button" class="secondary" onClick=${loadMore}>Load more</button></div>`}
  </div>`;
}

/** The item a [#id] reference or a task points at, in a card under the digest. */
function RefItem({ id, onClose }) {
  const [it, setIt] = useState(null);
  const [err, setErr] = useState("");
  useEffect(() => { setIt(null); setErr(""); get(`/api/inbox/items/${id}`).then(setIt, (e) => setErr(e.message)); }, [id]);
  return html`<div class="card ref-item">
    <div class="card-head"><strong>${it ? itemHead(it) : "#" + id}</strong>
      <button type="button" class="link" onClick=${onClose}>Close</button></div>
    ${err ? html`<p class="notice bad">${err}</p>` : !it ? html`<div class="muted">Loading…</div>` : html`<${ItemBody} it=${it} />`}
  </div>`;
}

function DigestCard({ d, currency, onRef }) {
  const onClick = (e) => {
    const b = /** @type {HTMLElement} */ (e.target).closest(".item-ref");
    if (b) onRef(Number(b.getAttribute("data-item")));
  };
  return html`<section class="card digest">
    <div class="digest-meta muted small">
      ${[fmtTime(d.created_at), d.item_count + " items", d.accounts.join(", "), d.executor + (d.model ? " (" + d.model + ")" : ""),
        d.cost ? fmtMoney(d.cost, currency) : ""].filter(Boolean).join(" · ")}
      ${d.session_id && html` · <a href=${"/s/" + d.session_id}>transcript</a>`}
    </div>
    ${d.state === "running" && html`<p class="notice">Writing the digest…</p>`}
    ${d.state === "failed" && html`<p class="notice bad">The digest failed: ${d.error}. Its items stay new.</p>`}
    ${d.state === "done" && d.error && html`<p class="notice warn small">${d.error}</p>`}
    ${d.html && html`<div class="md digest-text" onClick=${onClick} dangerouslySetInnerHTML=${{ __html: d.html }}></div>`}
  </section>`;
}

function DigestsTab({ app }) {
  const [d, setD] = useState(null);
  const [sel, setSel] = useState(null);
  const [ref, setRef] = useState(0);
  const [busy, setBusy] = useState(false);
  const load = () => get("/api/inbox/digests").then((r) => { setD(r); return r; }, (e) => toast(e.message));
  useEffect(() => { load(); }, []);
  const running = d && (d.running || (d.digests[0] && d.digests[0].state === "running"));
  useEffect(() => {
    if (!running) return;
    const t = setInterval(() => load().then((r) => {
      if (r && sel && r.digests.length && sel.id === r.digests[0].id) setSel(null);
    }), 3000);
    return () => clearInterval(t);
  }, [running]);
  if (!d) return html`<div class="muted">Loading…</div>`;
  const now = async () => {
    setBusy(true);
    try {
      await api("POST", "/api/inbox/digests", {});
      setSel(null);
      await load();
    } catch (e) { toast(e.message); } finally { setBusy(false); }
  };
  const open = (/** @type {number} */ id) => get(`/api/inbox/digests/${id}`).then((x) => { setSel(x); setRef(0); }, (e) => toast(e.message));
  const shown = sel || d.digests[0];
  return html`<div class="inbox-tab">
    <div class="section-row first-row">
      <div class="muted small">${d.schedule.length ? "Scheduled at " + d.schedule.join(", ") + " (" + d.timezone + ")" : "No schedule"}</div>
      <button type="button" onClick=${now} disabled=${busy || running || !d.has_new}
        title=${d.has_new ? "Summarize the new items now" : "No new items"}>${running ? "Running…" : "Digest now"}</button>
    </div>
    ${!shown ? html`<div class="muted">No digests yet.</div>` : html`<${DigestCard} d=${shown} currency=${app.me.currency} onRef=${setRef} />`}
    ${ref > 0 && html`<${RefItem} id=${ref} onClose=${() => setRef(0)} />`}
    ${d.digests.length > 1 && html`<h3 class="section-h">Earlier</h3>
      <ul class="list">${d.digests.map((x) => html`<li key=${x.id} class=${shown && shown.id === x.id ? "active" : ""}>
        <button type="button" class="link" onClick=${() => open(x.id)}>${fmtTime(x.created_at)}</button>
        <span class="muted small"> · ${x.item_count} items · ${x.state}</span></li>`)}</ul>`}
  </div>`;
}

/** A task's linked inbox items: a link to the source (new tab) when the item has a URL,
 *  plus its #id to open the item in the page. */
function TaskLinks({ ids, items, onRef }) {
  return html`<div class="task-links small">
    ${ids.map((id) => {
      const it = items[id];
      const url = it && /^https?:\/\//.test(it.url || "") ? it.url : "";
      return html`<div key=${id} class="task-link">
        <button type="button" class="item-ref" onClick=${() => onRef(id)}>#${id}</button>
        ${url ? html`<a href=${url} target="_blank" rel="noopener noreferrer">${itemHead(it)}</a>`
          : it ? html`<span class="muted">${itemHead(it)}</span>` : null}
      </div>`;
    })}
  </div>`;
}

function TaskRow({ t, items, onPatch, onRef }) {
  const reason = t.state === "proposed" ? firstLine(t.body) : (t.body || "").trim();
  return html`<li class="task">
    <div class="task-title">${t.title}${t.due && html` <span class="pill">${t.due}</span>`}
      ${t.source === "model" && t.state === "proposed" && html` <span class="pill accent">proposed by the digest</span>`}</div>
    ${reason && html`<div class="muted small task-body">${reason}</div>`}
    ${t.item_ids.length > 0 && html`<${TaskLinks} ids=${t.item_ids} items=${items} onRef=${onRef} />`}
    <div class="buttons">
      ${t.state === "proposed" && html`<button type="button" onClick=${() => onPatch(t, "open")}>Accept</button>
        <button type="button" class="secondary" onClick=${() => onPatch(t, "dismissed")}>Dismiss</button>`}
      ${t.state === "open" && html`<button type="button" onClick=${() => onPatch(t, "done")}>Done</button>`}
      ${(t.state === "done" || t.state === "dismissed") && html`<span class="pill">${t.state}</span>
        <button type="button" class="link" onClick=${() => onPatch(t, "open")}>Reopen</button>`}
    </div>
  </li>`;
}

function TasksTab() {
  const [ts, setTs] = useState(null);
  const [title, setTitle] = useState("");
  const [ref, setRef] = useState(0);
  const [items, setItems] = useState(/** @type {Record<number, any>} */ ({}));
  const asked = useRef(/** @type {Set<number>} */ (new Set()));
  const load = () => get("/api/inbox/tasks").then((r) => setTs(r.tasks), (e) => toast(e.message));
  useEffect(() => { load(); }, []);
  // The linked items of proposed and open tasks (title and URL for the links), each once.
  useEffect(() => {
    if (!ts) return;
    const ids = [...new Set(ts.filter((t) => t.state === "proposed" || t.state === "open").flatMap((t) => t.item_ids))]
      .filter((id) => !asked.current.has(id)).slice(0, 100);
    ids.forEach((id) => {
      asked.current.add(id);
      get(`/api/inbox/items/${id}`).then((it) => setItems((m) => ({ ...m, [id]: it })), () => {});
    });
  }, [ts]);
  const add = async (e) => {
    e.preventDefault();
    if (!title.trim()) return;
    try {
      await api("POST", "/api/inbox/tasks", { title });
      setTitle("");
      load();
    } catch (x) { toast(x.message); }
  };
  const onPatch = async (t, state) => {
    try {
      await api("PATCH", `/api/inbox/tasks/${t.id}`, { state });
      load();
    } catch (x) { toast(x.message); }
  };
  if (!ts) return html`<div class="muted">Loading…</div>`;
  const by = (/** @type {string[]} */ ...states) => ts.filter((t) => states.includes(t.state));
  const section = (/** @type {string} */ label, list) => list.length > 0 && html`<h3 class="section-h">${label} (${list.length})</h3>
    <ul class="list">${list.map((t) => html`<${TaskRow} key=${t.id} t=${t} items=${items} onPatch=${onPatch} onRef=${setRef} />`)}</ul>`;
  const closed = by("done", "dismissed");
  return html`<div class="inbox-tab">
    <form class="task-add" onSubmit=${add}>
      <input type="text" placeholder="Add a task" value=${title} maxlength="300" aria-label="New task"
        onInput=${(e) => setTitle(e.currentTarget.value)} />
      <button type="submit" disabled=${!title.trim()}>Add</button>
    </form>
    ${ref > 0 && html`<${RefItem} id=${ref} onClose=${() => setRef(0)} />`}
    ${section("Proposed", by("proposed"))}
    ${section("Open", by("open"))}
    ${ts.length === 0 && html`<div class="muted">No tasks.</div>`}
    ${ts.length > 0 && closed.length === ts.length && html`<div class="muted">Nothing to do.</div>`}
    ${closed.length > 0 && html`<details class="more"><summary>Show done (${closed.length})</summary>
      <ul class="list">${closed.map((t) => html`<${TaskRow} key=${t.id} t=${t} items=${items} onPatch=${onPatch} onRef=${setRef} />`)}</ul></details>`}
  </div>`;
}

function AccountEditor({ me, item, kinds, keys, onSaved, onClose }) {
  const tmpl = (/** @type {string} */ k) => {
    const kind = kinds.find((x) => x.name === k) || kinds[0];
    return JSON.stringify({ name: "", kind: kind.name, owner_sub: me.email || me.sub, interval_s: 900, enabled: true,
      key_ref: "", config: kind.template }, null, 2);
  };
  const initial = item ? JSON.stringify({ name: item.name, kind: item.kind, owner_sub: item.owner_sub, interval_s: item.interval_s,
    enabled: item.enabled, key_ref: item.key_ref || "", config: item.config }, null, 2) : tmpl(kinds[0] ? kinds[0].name : "rss");
  const [text, setText] = useState(initial);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const readOnly = item && !item.editable;
  const save = async (e) => {
    e.preventDefault();
    let body;
    try { body = JSON.parse(text); } catch (x) { setErr("not valid JSON: " + x.message); return; }
    setBusy(true);
    setErr("");
    try {
      const r = item ? await api("PATCH", `/api/inbox/accounts/${item.id}`, body) : await api("POST", "/api/inbox/accounts", body);
      toast("saved " + r.name, "info");
      onSaved();
    } catch (x) { setErr(x.message); } finally { setBusy(false); }
  };
  return html`<section class="editor card">
    <div class="card-head"><h2>${item ? "Account " + item.name : "New account"}</h2>
      <button type="button" class="link" onClick=${onClose}>Close</button></div>
    ${readOnly && html`<p class="notice warn">Defined in hub.toml, which is re-read on every hub start; edit it there.</p>`}
    ${!item && html`<div class="buttons kind-pick">${kinds.map((k) => html`<button type="button" key=${k.name} class="secondary"
      onClick=${() => setText(tmpl(k.name))}>${k.name}</button>`)}</div>`}
    <form class="stack" onSubmit=${save}>
      <label>Account JSON (DESIGN §23.2, §23.4)
        <textarea class="code" rows="14" spellcheck="false" readonly=${readOnly} value=${text}
          onInput=${(e) => setText(e.currentTarget.value)}></textarea></label>
      ${err && html`<p class="notice bad" role="alert">${err}</p>`}
      <p class="muted small">owner_sub: a user's sub or e-mail. interval_s ≥ 60. Secrets stay in the secrets file and are named by
        <code>key_ref</code>. Known key_refs: ${keys.length ? keys.map((k) => html`<code key=${k}>${k}</code> `) : "none"}</p>
      ${!readOnly && html`<div><button type="submit" disabled=${busy}>Save</button></div>`}
    </form>
  </section>`;
}

function AccountsTab({ app, data, reload }) {
  const [edit, setEdit] = useState(""); // account id, "new" or ""
  const [busy, setBusy] = useState("");
  const accounts = data.accounts;
  const poll = async (a) => {
    setBusy(a.id);
    try {
      const r = await api("POST", `/api/inbox/accounts/${a.id}/poll`);
      toast(`${a.name}: ${r.items_added} new`, "info");
    } catch (e) { toast(e.message); } finally { setBusy(""); reload(); }
  };
  const toggle = async (a) => {
    try { await api("PATCH", `/api/inbox/accounts/${a.id}`, { enabled: !a.enabled }); reload(); } catch (e) { toast(e.message); }
  };
  const remove = async (a) => {
    if (!confirm(`Delete ${a.name} and its ${a.total_items} items?`)) return;
    try { await api("DELETE", `/api/inbox/accounts/${a.id}`); setEdit(""); reload(); } catch (e) { toast(e.message); }
  };
  const item = edit && edit !== "new" ? accounts.find((a) => a.id === edit) : null;
  return html`<div class="inbox-tab">
    ${(edit === "new" || item) && html`<${AccountEditor} key=${edit} me=${app.me} item=${item} kinds=${data.kinds || []} keys=${data.keys || []}
      onSaved=${() => { setEdit(""); reload(); }} onClose=${() => setEdit("")} />`}
    <div class="section-row first-row"><span class="muted small">${accounts.length} accounts</span>
      <button type="button" class="secondary" onClick=${() => setEdit("new")}>New account</button></div>
    <ul class="list">${accounts.length === 0 ? html`<li class="muted">none</li>` : accounts.map((a) => html`<li key=${a.id} class=${edit === a.id ? "active" : ""}>
      <div class="acct-head"><strong>${a.name}</strong> <span class="pill">${a.kind}</span>
        ${!a.enabled && html` <span class="pill bad">disabled</span>`}
        ${a.source === "config" && html` <span class="muted small">· hub.toml</span>`}</div>
      <div class="muted small">${a.owner}${a.owner_resolved ? "" : " (not logged in yet)"} · ${a.new_items} new / ${a.total_items}
        ${a.pollable && html` · every ${Math.round(a.interval_s / 60)} min`}${a.backoff_s ? ` · backoff ${Math.round(a.backoff_s / 60)} min` : ""}</div>
      ${a.pollable && html`<div class="muted small">last run ${a.last_run ? fmtTime(a.last_run) : "never"}${a.last_ok ? " · last ok " + fmtTime(a.last_ok) : ""}</div>`}
      ${a.ingest_path && html`<div class="small acct-ingest">POST <code>${a.ingest_path}</code></div>`}
      ${a.last_error && html`<div class="notice bad small">${a.last_error}</div>`}
      <div class="buttons">
        ${a.pollable && html`<button type="button" class="secondary" disabled=${busy === a.id} onClick=${() => poll(a)}>${busy === a.id ? "Polling…" : "Poll now"}</button>`}
        ${a.editable && html`<button type="button" class="secondary" onClick=${() => toggle(a)}>${a.enabled ? "Disable" : "Enable"}</button>`}
        <button type="button" class="secondary" onClick=${() => setEdit(a.id)}>${a.editable ? "Edit" : "View"}</button>
        ${a.editable && html`<button type="button" class="deny" onClick=${() => remove(a)}>Delete</button>`}
      </div>
    </li>`)}</ul>
  </div>`;
}

export function InboxView({ app, search }) {
  const params = new URLSearchParams(search);
  let tab = params.get("tab") || "items";
  if (tab === "accounts" && !app.me.is_admin) tab = "items";
  const [acc, setAcc] = useState(null);
  const loadAccounts = () => get("/api/inbox/accounts").then(setAcc, (e) => toast(e.message));
  useEffect(() => { loadAccounts(); }, []);
  const go = (/** @type {string} */ t) => app.navigate("/inbox" + (t === "items" ? "" : "?tab=" + t), true);
  const shownTabs = tabs.filter(([k]) => k !== "accounts" || app.me.is_admin);
  return html`<${Page} app=${app} title="Inbox">
    <div class="tabs inbox-tabs" role="tablist">
      ${shownTabs.map(([k, label]) => html`<button type="button" role="tab" key=${k} aria-selected=${tab === k}
        class=${cx("tab", tab === k && "active")} onClick=${() => go(k)}>${label}</button>`)}
    </div>
    ${!acc ? html`<div class="muted">Loading…</div>`
      : tab === "digests" ? html`<${DigestsTab} app=${app} />`
      : tab === "tasks" ? html`<${TasksTab} />`
      : tab === "accounts" ? html`<${AccountsTab} app=${app} data=${acc} reload=${loadAccounts} />`
      : html`<${ItemsTab} app=${app} accounts=${acc.accounts} />`}
  <//>`;
}
