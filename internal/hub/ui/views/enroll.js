// /admin/enroll: a one-time code for `mado enroll` on a new machine (15 minutes).
import { html, useState } from "../lib.js";
import { api } from "../api.js";
import { Page } from "./page.js";

export function EnrollView({ app }) {
  const [name, setName] = useState("");
  const [r, setR] = useState(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    setErr("");
    try { setR(await api("POST", "/api/admin/enroll", { name: name.trim() })); } catch (x) { setErr(x.message); setR(null); }
    setBusy(false);
  };
  return html`<${Page} app=${app} title="Enroll a machine">
    <p class="muted"><a href="/admin/machines">All machines</a></p>
    <form class="stack" onSubmit=${submit} autocomplete="off">
      <label>Machine name
        <input value=${name} maxlength="64" required pattern="[A-Za-z0-9_\\-]+" onInput=${(e) => setName(e.currentTarget.value)} />
      </label>
      ${err && html`<p class="notice bad" role="alert">${err}</p>`}
      <div><button type="submit" disabled=${busy}>Create code</button></div>
    </form>
    ${r && html`<section class="card enroll-result">
      <p>One-time code for <strong>${r.name}</strong>, valid ${r.minutes} minutes. On the machine run:</p>
      <pre class="cmd">${r.command}</pre>
    </section>`}
  <//>`;
}
