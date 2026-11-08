// /admin/users (DESIGN §17.6): every user with the admin flag (from the IdP group,
// read-only), last login, this month's cost and budget; admins edit the budget.
import { html, useState, useEffect } from "../lib.js";
import { api, get } from "../api.js";
import { toast } from "../components/toast.js";
import { Page } from "./page.js";
import { fmtMoney, fmtTime } from "../util.js";

function BudgetEditor({ u, currency, onSaved, onCancel }) {
  const [v, setV] = useState(u.own_budget === null ? "" : String(u.own_budget));
  const [busy, setBusy] = useState(false);
  const save = async (/** @type {number|null} */ value) => {
    setBusy(true);
    try {
      onSaved(await api("PATCH", `/api/admin/users/${encodeURIComponent(u.sub)}`, { monthly_budget: value }));
    } catch (e) {
      toast(e.message);
      setBusy(false);
    }
  };
  const submit = (e) => {
    e.preventDefault();
    const n = Number(v);
    if (v.trim() === "" || !isFinite(n) || n < 0) { toast("enter a number >= 0"); return; }
    save(n);
  };
  return html`<form class="budget-edit" onSubmit=${submit}>
    <input type="number" min="0" step=${currency === "JPY" ? "1" : "0.01"} value=${v} aria-label="monthly budget"
      onInput=${(e) => setV(e.currentTarget.value)} placeholder=${currency} />
    <button type="submit" disabled=${busy}>Save</button>
    <button type="button" class="secondary" disabled=${busy} onClick=${() => save(null)} title="Use [budgets] default_monthly">Default</button>
    <button type="button" class="link" onClick=${onCancel}>Cancel</button>
  </form>`;
}

export function AdminUsersView({ app }) {
  const [d, setD] = useState(null);
  const [editing, setEditing] = useState("");
  useEffect(() => { get("/api/admin/users").then(setD, (e) => toast(e.message)); }, []);
  if (!app.me.is_admin) return html`<${Page} app=${app} title="Users"><p class="muted">Admins only.</p><//>`;
  if (!d) return html`<${Page} app=${app} title="Users" wide><div class="muted">Loading…</div><//>`;
  const cur = d.currency;
  const money = (/** @type {number} */ x) => fmtMoney(x, cur);
  const saved = (/** @type {any} */ u) => {
    setD((x) => ({ ...x, users: x.users.map((o) => (o.sub === u.sub ? u : o)) }));
    setEditing("");
    toast("budget saved", "info");
  };
  const over = d.users.filter((u) => u.over_budget).length;
  return html`<${Page} app=${app} title="Users" wide>
    <p class="muted">${d.users.length} users. Budgets are per UTC calendar month (${d.month}) in ${cur};
      the default is ${d.default_budget === null ? "no limit" : money(d.default_budget)} ([budgets] default_monthly in hub.toml).
      The admin flag comes from the IdP group at each login.</p>
    ${over > 0 && html`<p class="notice bad">${over} ${over === 1 ? "user is" : "users are"} over budget.</p>`}
    <div class="table-wrap"><table class="data users">
      <thead><tr><th>user</th><th>role</th><th>last login</th><th class="num">this month</th><th class="num">budget</th><th>status</th><th></th></tr></thead>
      <tbody>${d.users.map((u) => html`<tr key=${u.sub} class=${u.over_budget ? "over" : ""}>
        <td class="u-main"><div class="u-cell"><strong>${u.name || u.email || u.sub}</strong>
          ${u.name && html`<span class="muted">${u.email}</span>`}</div></td>
        <td data-label="role">${u.is_admin ? html`<span class="pill accent">admin</span>` : html`<span class="pill">user</span>`}</td>
        <td data-label="last login">${fmtTime(u.last_login) || html`<span class="muted">never</span>`}</td>
        <td class="num" data-label="this month"><a href=${"/usage?user=" + encodeURIComponent(u.sub)}>${money(u.month_cost)}</a></td>
        <td class="num" data-label="budget">${editing === u.sub
          ? html`<${BudgetEditor} u=${u} currency=${cur} onSaved=${saved} onCancel=${() => setEditing("")} />`
          : html`<span>${u.budget === null ? html`<span class="muted">no limit</span>` : money(u.budget)}${u.own_budget === null &&
              html` <span class="muted small">(default)</span>`}</span>`}</td>
        <td data-label="status">${u.budget === null ? html`<span class="pill">unlimited</span>`
          : u.over_budget ? html`<span class="pill bad">over budget</span>` : html`<span class="pill ok">ok</span>`}</td>
        <td class="u-act">${editing !== u.sub && html`<button type="button" class="secondary" onClick=${() => setEditing(u.sub)}>Edit budget</button>`}</td>
      </tr>`)}</tbody>
    </table></div>
  <//>`;
}
