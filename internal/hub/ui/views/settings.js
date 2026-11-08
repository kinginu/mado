// /settings: the user's own account, role and this month's budget. Nothing to edit
// yet; the role comes from the IdP group, the budget from an admin.
import { html, useState, useEffect } from "../lib.js";
import { get } from "../api.js";
import { toast } from "../components/toast.js";
import { Page, Stat } from "./page.js";
import { fmtMoney } from "../util.js";

function Row({ k, children }) {
  return html`<div class="kv"><span class="k">${k}</span><span class="v">${children}</span></div>`;
}

export function SettingsView({ app }) {
  const [me, setMe] = useState(app.me);
  useEffect(() => { get("/api/me").then(setMe, (e) => toast(e.message)); }, []);
  const b = me.budget || {};
  const money = (/** @type {number} */ x) => fmtMoney(x, me.currency);
  const left = b.budget === null || b.budget === undefined ? null : Math.max(0, b.budget - b.month_cost);
  return html`<${Page} app=${app} title="Settings">
    <section class="card">
      <h3 class="section-h first">Account</h3>
      <${Row} k="name">${me.name || html`<span class="muted">not set</span>`}<//>
      <${Row} k="email">${me.email}<//>
      <${Row} k="role">${me.is_admin ? html`<span class="pill accent">admin</span>` : html`<span class="pill">user</span>`}
        <span class="muted small"> from the sign-in provider's group, updated at each login</span><//>
      <${Row} k="account id"><code>${me.sub}</code><//>
    </section>
    <h3 class="section-h">This month</h3>
    <div class="stats">
      <${Stat} label="Spent" value=${money(b.month_cost)} tone=${b.over_budget ? "bad" : ""}
        note=${b.over_budget ? "budget used up" : "UTC calendar month"} />
      <${Stat} label="Budget" value=${b.budget === null || b.budget === undefined ? "no limit" : money(b.budget)}
        note=${b.own_budget === null || b.own_budget === undefined ? "the hub's default" : "set for you by an admin"} />
      ${left !== null && html`<${Stat} label="Left" value=${money(left)} />`}
    </div>
    ${b.over_budget && html`<p class="notice bad">New chat messages and router code sessions are refused until the month
      ends. Subscription sessions still work. Ask an admin to raise the budget.</p>`}
    <p class="muted small">Costs are in ${me.currency}. Details by model are on the <a href="/usage">usage page</a>.</p>
  <//>`;
}
