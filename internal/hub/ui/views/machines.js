// /admin/machines (DESIGN §24): the enrolled machines, with Remove for offline ones.
// The hub refuses (409) while the worker is connected; the reason is shown as is.
// Sessions there that have not ended are marked ended by the removal, so the confirm
// dialog warns with the live_sessions count the admin's machine list carries.
import { html, useState, useEffect } from "../lib.js";
import { api, get } from "../api.js";
import { toast } from "../components/toast.js";
import { Page } from "./page.js";
import { fmtTime } from "../util.js";

export function MachinesView({ app }) {
  const [ms, setMs] = useState(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState("");
  const load = () => get("/api/machines").then((r) => setMs(r.machines || []), (e) => toast(e.message));
  useEffect(() => { load(); }, []);
  if (!app.me.is_admin) return html`<${Page} app=${app} title="Machines"><p class="muted">Admins only.</p><//>`;
  const remove = async (/** @type {{name:string, live_sessions?:number}} */ m) => {
    const n = m.live_sessions || 0;
    const warn = n === 0 ? "" : n === 1
      ? "\n\n1 session on this machine is still idle/running and will be marked ended."
      : `\n\n${n} sessions on this machine are still idle/running and will be marked ended.`;
    if (!confirm(`Remove machine "${m.name}"? Its token stops working; its old sessions stay. Enroll the name again to reuse it.${warn}`)) return;
    setBusy(m.name);
    setErr("");
    try {
      await api("DELETE", `/api/machines/${encodeURIComponent(m.name)}`);
      toast(n ? `removed ${m.name}; ended ${n} session${n === 1 ? "" : "s"}` : `removed ${m.name}`, "info");
    } catch (e) {
      setErr(`${m.name}: ${e.message}`);
    }
    setBusy("");
    load();
  };
  return html`<${Page} app=${app} title="Machines" wide>
    <p class="muted">Workers enrolled with this hub. <a href="/admin/enroll">Enroll a machine</a>.
      A machine can be removed once its worker is stopped; sessions still live there are marked ended.</p>
    ${err && html`<p class="notice bad" role="alert">${err}</p>`}
    ${!ms ? html`<div class="muted">Loading…</div>`
      : ms.length === 0 ? html`<p class="muted">No machines enrolled.</p>`
      : html`<div class="table-wrap"><table class="data users">
      <thead><tr><th>machine</th><th>status</th><th>last seen</th><th>version</th><th></th></tr></thead>
      <tbody>${ms.map((m) => html`<tr key=${m.id}>
        <td class="u-main"><strong>${m.name}</strong></td>
        <td data-label="status">${m.online ? html`<span class="pill ok">online</span>` : html`<span class="pill">offline</span>`}</td>
        <td data-label="last seen">${fmtTime(m.last_seen) || html`<span class="muted">never</span>`}</td>
        <td data-label="version">${m.version || html`<span class="muted">-</span>`}${m.claude_version && html` <span class="muted small">claude ${m.claude_version}</span>`}</td>
        <td class="u-act">${!m.online && html`<button type="button" class="secondary" disabled=${busy === m.name}
          onClick=${() => remove(m)}>Remove</button>`}</td>
      </tr>`)}</tbody>
    </table></div>`}
  <//>`;
}
