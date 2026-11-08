// Left sidebar (DESIGN §17.5): new chat / new code session, the inbox, search, recents
// with kind icon (chat, code, concierge, digest) and live state dot, admin links, user
// menu with logout. Digest sessions (DESIGN §23.5) are hidden unless the filter is on.
import { html, useState } from "../lib.js";
import { getCSRF } from "../api.js";
import { IconChat, IconCode, IconDigest, IconInbox, IconPlan, IconPlus, IconSearch } from "./icons.js";
import { baseState, cx } from "../util.js";

function StateDot({ s }) {
  const st = baseState(s.state);
  const cls = st === "waiting_approval" ? "wait" : s.running || st === "running" || st === "starting" ? "live" : st === "exited" || st === "detached" ? "off" : "";
  if (!cls) return null;
  return html`<span class=${"dot " + cls} title=${s.state}></span>`;
}

function readDigestPref() {
  try { return localStorage.getItem("mado.digests") === "shown"; } catch (e) { return false; }
}

export function Sidebar({ me, sessions, current, path }) {
  const [q, setQ] = useState("");
  const [menu, setMenu] = useState(false);
  const [digests, setDigests] = useState(readDigestPref);
  const needle = q.trim().toLowerCase();
  const hasDigests = sessions.some((s) => s.kind === "digest");
  const visible = digests ? sessions : sessions.filter((s) => s.kind !== "digest" || s.id === current);
  const shown = needle ? visible.filter((s) => (s.title || "").toLowerCase().includes(needle)) : visible;
  const toggleDigests = (/** @type {boolean} */ v) => {
    setDigests(v);
    try { localStorage.setItem("mado.digests", v ? "shown" : "hidden"); } catch (e) { /* private mode */ }
  };
  const link = (href, label) => html`<a class=${cx("nav", (path === href || path.startsWith(href + "/")) && "active")} href=${href}>${label}</a>`;
  return html`<aside class="sidebar" aria-label="sessions">
    <div class="side-head"><a class="brand" href="/">mado</a></div>
    <div class="side-new">
      ${me.chat_enabled && html`<a class="new-btn" href="/new?kind=chat"><${IconPlus} /> New chat</a>`}
      <a class=${cx("new-btn", me.chat_enabled && "secondary")} href="/new?kind=code"><${IconCode} /> New code session</a>
      ${me.inbox && html`<a class=${cx("new-btn", "secondary", path === "/inbox" && "active")} href="/inbox"><${IconInbox} /> Inbox</a>`}
    </div>
    <label class="search"><${IconSearch} />
      <input type="search" placeholder="Search" value=${q} onInput=${(e) => setQ(e.currentTarget.value)} aria-label="Search sessions" />
    </label>
    <div class="side-label side-label-row">Recents
      ${hasDigests && html`<label class="digest-toggle" title="Show inbox digest runs">
        <input type="checkbox" checked=${digests} onChange=${(e) => toggleDigests(e.currentTarget.checked)} /> digests</label>`}</div>
    <nav class="recents">
      ${shown.length === 0 && html`<div class="muted empty">${needle ? "No match" : "No sessions yet"}</div>`}
      ${shown.map((s) => html`<a key=${s.id} href=${"/s/" + s.id} class=${cx("recent", s.id === current && "active")} title=${s.title}>
        ${s.concierge ? html`<span class="kind-plan" title="concierge"><${IconPlan} /></span>` : s.kind === "digest" ? html`<span title="inbox digest"><${IconDigest} /></span>` : s.kind === "chat" ? html`<${IconChat} />` : html`<${IconCode} />`}
        <span class="r-title">${s.title || "Untitled"}</span>
        <${StateDot} s=${s} />
      </a>`)}
    </nav>
    <div class="side-links">
      ${link("/usage", "Usage")}
      ${me.is_admin && html`${link("/admin/users", "Users")}
        ${link("/profiles", "Profiles & chat models")}
        ${link("/history", "History")}
        ${link("/admin/machines", "Machines")}`}
      ${link("/audit", "Audit")}
    </div>
    <div class="user">
      <button type="button" class="user-btn" aria-expanded=${menu} onClick=${() => setMenu(!menu)}>
        <span class="avatar">${(me.name || me.email || "?").slice(0, 1).toUpperCase()}</span>
        <span class="u-name">${me.name || me.email}</span>
        ${me.is_admin && html`<span class="badge">admin</span>`}
      </button>
      ${menu && html`<div class="user-menu">
        <div class="muted u-email">${me.email}</div>
        ${link("/settings", "Settings")}
        <form method="post" action="/logout">
          <input type="hidden" name="csrf" value=${getCSRF()} />
          <button type="submit" class="link">Log out</button>
        </form>
      </div>`}
    </div>
  </aside>`;
}
