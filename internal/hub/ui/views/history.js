// /history and /history/{id} (admins): Claude Code transcripts imported read-only from
// the machines (DESIGN §14). The file view renders the lines with the session view's
// transcript (same blocks, same tool chunks); "Resume in mado" starts a subscription
// session with claude --resume.
import { html, useState, useEffect } from "../lib.js";
import { api, get } from "../api.js";
import { toast } from "../components/toast.js";
import { Transcript } from "../components/transcript.js";
import { Page } from "./page.js";
import { TopBar } from "./session.js";
import { fmtBytes } from "../util.js";

export function HistoryListView({ app, search }) {
  const q = new URLSearchParams(search).get("q") || "";
  const [text, setText] = useState(q);
  const [d, setD] = useState(null);
  useEffect(() => {
    setText(q);
    get("/api/admin/history" + (q ? "?q=" + encodeURIComponent(q) : "")).then(setD, (e) => toast(e.message));
  }, [q]);
  const rescan = async (/** @type {string} */ id) => {
    try { await api("POST", "/api/admin/history/rescan", { machine: id }); toast("rescan requested", "info"); } catch (e) { toast(e.message); }
  };
  const submit = (e) => {
    e.preventDefault();
    app.navigate("/history" + (text.trim() ? "?q=" + encodeURIComponent(text.trim()) : ""), true);
  };
  const t = d && d.totals;
  return html`<${Page} app=${app} title="Claude Code history" wide>
    <p class="muted">Transcripts imported read-only from machines with <code>claude_history = true</code>.${" "}
      ${t && `${t.files} files, ${t.lines} lines, ${fmtBytes(t.bytes)}${t.bad ? `, ${t.bad} unparsable lines` : ""}. `}Visible to admins only for now.</p>
    <form class="toolbar" onSubmit=${submit} role="search">
      <label class="grow">Search
        <input type="search" value=${text} maxlength="200" placeholder="Title or directory" onInput=${(e) => setText(e.currentTarget.value)} />
      </label>
      <button type="submit">Search</button>
    </form>
    ${!d ? html`<div class="muted">Loading…</div>` : d.groups.length === 0
      ? html`<p class="muted">${q ? "No match." : "Nothing imported yet."}</p>`
      : d.groups.map((g) => html`<section key=${g.machine_id}>
          <div class="section-row"><h3 class="section-h">${g.machine}
            ${g.online ? html` <span class="pill ok">online</span>` : html` <span class="pill">offline</span>`}</h3>
            ${g.online && html`<button type="button" class="secondary" onClick=${() => rescan(g.machine_id)}>Rescan now</button>`}</div>
          <ul class="list">${g.files.map((f) => html`<li key=${f.id}>
            <a href=${"/history/" + f.id}><strong>${f.title || f.session_id.slice(0, 8)}</strong></a>
            <div class="muted small">${f.cwd} · ${f.modified} · ${f.lines} lines${f.default_config ? "" : " · " + f.config_dir}</div>
          </li>`)}</ul>
        </section>`)}
  <//>`;
}

export function HistoryFileView({ app, id }) {
  const [d, setD] = useState(null);
  const [blocks, setBlocks] = useState([]);
  const [busy, setBusy] = useState(false);
  const load = async (/** @type {string} */ q, /** @type {boolean} */ append) => {
    setBusy(true);
    try {
      const r = await get(`/api/admin/history/${encodeURIComponent(id)}${q}`);
      setD(r);
      setBlocks((xs) => (append ? xs.concat(r.blocks) : r.blocks));
    } catch (e) {
      toast(e.message);
      if (e.status === 404) setD({ missing: true });
    } finally {
      setBusy(false);
    }
  };
  useEffect(() => { setD(null); setBlocks([]); load("", false); }, [id]);
  if (!d) return html`<${Page} app=${app} title="History"><div class="muted">Loading…</div><//>`;
  if (d.missing) return html`<${Page} app=${app} title="History"><p class="muted">No such transcript.</p><a class="button" href="/history">All history</a><//>`;
  const f = d.file;
  const resume = async () => {
    try {
      const r = await api("POST", `/api/admin/history/${f.id}/resume`);
      app.refreshSessions();
      app.navigate("/s/" + r.id);
    } catch (e) { toast(e.message); }
  };
  return html`<div class="main">
    <${TopBar} app=${app} title=${f.title || f.session_id} />
    <div class="scroller">
      <div class="history-head">
        <div class="muted small">${f.machine}${d.online ? "" : " (offline)"} · <code>${f.cwd}</code> · ${f.modified} · ${f.lines} lines${f.bad_lines ? ` (${f.bad_lines} unparsable)` : ""}</div>
        <div class="muted small"><code>${f.session_id}</code></div>
        <div class="buttons">
          <a class="button secondary" href="/history">All history</a>
          ${!d.first && html`<button type="button" class="secondary" disabled=${busy} onClick=${() => load("", false)}>From the start</button>`}
          <button type="button" class="secondary" disabled=${busy} onClick=${() => load("?tail=1", false)}>Jump to the end</button>
          ${f.default_config
            ? html`<button type="button" disabled=${!d.online} onClick=${resume}
                title=${`Start a mado session with claude --resume on ${f.machine} (subscription profile)`}>Resume in mado</button>`
            : html`<span class="muted small">From ${f.config_dir}: not resumable with the subscription profile.</span>`}
        </div>
      </div>
      <${Transcript} sessionID=${"history-" + f.id} blocks=${blocks} approvals=${[]} drafts=${{}}
        running=${false} showNoise=${false} onDecided=${() => {}} />
      ${d.more && html`<div class="log"><button type="button" class="secondary" disabled=${busy}
        onClick=${() => load("?after=" + d.last, true)}>Load the next 300 lines</button></div>`}
    </div>
  </div>`;
}
