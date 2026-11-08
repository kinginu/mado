// /usage (DESIGN §17.6): tokens and cost of a month by model and kind, the CSV export.
// Admins pick a user or see everyone, with totals per user.
import { html, useState, useEffect } from "../lib.js";
import { get } from "../api.js";
import { toast } from "../components/toast.js";
import { Page, Stat, personLabel } from "./page.js";
import { fmtMoney, fmtTokens, shortModel } from "../util.js";

function monthLabel(/** @type {string} */ m) {
  const [y, mo] = m.split("-").map(Number);
  return new Date(Date.UTC(y, mo - 1, 1)).toLocaleDateString(undefined, { year: "numeric", month: "long", timeZone: "UTC" });
}

const num = (/** @type {number} */ n) => (n || 0).toLocaleString("en-US");

export function UsageView({ app, search }) {
  const params = new URLSearchParams(search);
  const month = params.get("month") || "";
  const user = params.get("user") || "";
  const [d, setD] = useState(null);
  useEffect(() => {
    const q = new URLSearchParams();
    if (month) q.set("month", month);
    if (user) q.set("user", user);
    get("/api/usage?" + q).then(setD, (e) => toast(e.message));
  }, [month, user]);

  const go = (/** @type {string} */ m, /** @type {string} */ u) => {
    const q = new URLSearchParams();
    if (m) q.set("month", m);
    if (u) q.set("user", u);
    const s = q.toString();
    app.navigate("/usage" + (s ? "?" + s : ""), true);
  };
  if (!d) return html`<${Page} app=${app} title="Usage" wide><div class="muted">Loading…</div><//>`;

  const cur = d.currency;
  const money = (/** @type {number} */ x) => fmtMoney(x, cur);
  const t = d.total || {};
  const people = d.people || [];
  const byID = Object.fromEntries(people.map((p) => [p.sub, p]));
  const csvQ = new URLSearchParams({ month: d.month });
  if (d.user) csvQ.set("user", d.user);
  const b = d.budget;
  const who = app.me.is_admin ? (d.user ? personLabel(byID[d.user], d.user) : "Everyone") : "";

  return html`<${Page} app=${app} title="Usage" wide>
    <div class="toolbar">
      <label>Month
        <select value=${d.month} onChange=${(e) => go(e.currentTarget.value, user)}>
          ${d.months.map((m) => html`<option key=${m} value=${m}>${monthLabel(m)}</option>`)}
        </select>
      </label>
      ${app.me.is_admin && html`<label>User
        <select value=${user} onChange=${(e) => go(month, e.currentTarget.value)}>
          <option value="">Everyone</option>
          ${people.map((p) => html`<option key=${p.sub} value=${p.sub}>${personLabel(p, p.sub)}${p.sub === app.me.sub ? " (you)" : ""}</option>`)}
        </select>
      </label>`}
      <span class="spacer"></span>
      <a class="button secondary" href=${"/api/usage.csv?" + csvQ} download>Export CSV</a>
    </div>
    ${who && html`<h2 class="page-h">${who} · ${monthLabel(d.month)}</h2>`}
    <div class="stats">
      <${Stat} label="Cost" value=${money(t.cost)}
        note=${b ? (b.budget === null ? "no budget limit" : `of ${money(b.budget)} budget`) : `${num(t.responses)} responses`}
        tone=${b && b.over_budget ? "bad" : ""} />
      <${Stat} label="Input" value=${fmtTokens(t.input)} note="tokens, without cache" />
      <${Stat} label="Cache" value=${fmtTokens((t.cache_read || 0) + (t.cache_write || 0))}
        note=${`${fmtTokens(t.cache_read)} read · ${fmtTokens(t.cache_write)} written`} />
      <${Stat} label="Output" value=${fmtTokens(t.output)} note=${`${num(t.responses)} responses`} />
    </div>
    ${b && b.over_budget && html`<p class="notice bad">The monthly budget is used up: new chat messages and router code
      sessions are refused until the month ends (UTC). Subscription sessions still work.</p>`}

    <h3 class="section-h">By model</h3>
    ${d.models.length === 0 ? html`<p class="muted">No usage in ${monthLabel(d.month)}.</p>` : html`<div class="table-wrap"><table class="data">
      <thead><tr><th>model</th><th class="num">cost</th><th class="num">resp.</th><th class="num">input</th><th class="num">cache read</th>
        <th class="num">cache write</th><th class="num">output</th></tr></thead>
      <tbody>${d.models.map((r) => html`<tr key=${r.model + r.kind}>
        <td title=${r.model}>${shortModel(r.model) || "?"} <span class="pill">${r.kind}</span></td><td class="num">${money(r.cost)}</td>
        <td class="num">${num(r.responses)}</td><td class="num">${num(r.input)}</td><td class="num">${num(r.cache_read)}</td>
        <td class="num">${num(r.cache_write)}</td><td class="num">${num(r.output)}</td></tr>`)}</tbody>
      <tfoot><tr><th>total</th><th class="num">${money(t.cost)}</th><th class="num">${num(t.responses)}</th><th class="num">${num(t.input)}</th>
        <th class="num">${num(t.cache_read)}</th><th class="num">${num(t.cache_write)}</th><th class="num">${num(t.output)}</th></tr></tfoot>
    </table></div>`}

    ${d.users && html`<h3 class="section-h">By user</h3>
      ${d.users.length === 0 ? html`<p class="muted">Nobody used anything.</p>` : html`<div class="table-wrap"><table class="data">
        <thead><tr><th>user</th><th class="num">cost</th><th class="num">resp.</th><th class="num">in (with cache)</th><th class="num">output</th></tr></thead>
        <tbody>${d.users.map((r) => html`<tr key=${r.sub}>
          <td><a href=${`/usage?month=${d.month}&user=${encodeURIComponent(r.sub)}`}>${personLabel(byID[r.sub], r.sub)}</a></td>
          <td class="num">${money(r.cost)}</td><td class="num">${num(r.responses)}</td>
          <td class="num">${fmtTokens(r.input + r.cache_read + r.cache_write)}</td><td class="num">${fmtTokens(r.output)}</td></tr>`)}</tbody>
      </table></div>`}`}
    <p class="muted small">Costs come from the hub's price table at the time of each response; subscription sessions
      count tokens at no cost. Months are UTC calendar months.</p>
  <//>`;
}
