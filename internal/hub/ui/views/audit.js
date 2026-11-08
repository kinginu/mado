// /audit: a user's own actions and the hub's automatic actions on their sessions;
// admins see the hub-wide log (?mine=1 narrows it). Message contents are never logged.
import { html, useState, useEffect } from "../lib.js";
import { get } from "../api.js";
import { toast } from "../components/toast.js";
import { Page } from "./page.js";
import { fmtTime } from "../util.js";

export function AuditView({ app, search }) {
  const mine = new URLSearchParams(search).get("mine") === "1";
  const [d, setD] = useState(null);
  useEffect(() => { setD(null); get("/api/audit" + (mine ? "?mine=1" : "")).then(setD, (e) => toast(e.message)); }, [mine]);
  return html`<${Page} app=${app} title="Audit" wide>
    <p class="muted">${d && d.all ? "Hub-wide log, newest first (last 1000)." :
      "Your actions and the hub's automatic actions on your sessions, newest first (last 500)."} Message contents are never logged.
      ${app.me.is_admin && (mine ? html` <a href="/audit">Everyone</a>` : html` <a href="/audit?mine=1">Only mine</a>`)}</p>
    ${!d ? html`<div class="muted">Loading…</div>` : d.rows.length === 0 ? html`<p class="muted">Nothing yet.</p>` :
      html`<ul class="list audit">${d.rows.map((r, i) => html`<li key=${i}>
        <strong>${r.action}</strong> <span class="muted small">${fmtTime(r.ts)} · ${r.user ? (d.all ? r.user : "you") : "hub"}</span>
        ${r.detail && html`<div><code>${r.detail}</code></div>`}
      </li>`)}</ul>`}
  <//>`;
}
