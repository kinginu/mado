// Right panel (DESIGN §17.5). Code: machine, cwd, profile, model, permission mode,
// the Models panel and pending approvals. Chat: model, tokens and cost this session
// and this month, attachments. Session actions for both.
import { html, useState, useEffect } from "../lib.js";
import { get } from "../api.js";
import { fmtTokens, fmtMoney, fmtBytes, shortModel, baseState, modeOptionLabel, fmtClock } from "../util.js";
import { ApprovalCard, questionsOf, Markdown } from "./transcript.js";

function Row({ k, children }) {
  return html`<div class="kv"><span class="k">${k}</span><span class="v">${children}</span></div>`;
}

/** The Models panel (DESIGN §15): usage.Stats as JSON (Rows, AgentCalls, Tokens). */
export function ModelsPanel({ stats }) {
  const rows = (stats && stats.Rows) || [];
  const tokens = (stats && stats.Tokens) || [];
  const calls = (stats && stats.AgentCalls) || 0;
  const short = rows.map((r) => `${shortModel(r.Model)} ×${r.Events}${r.Subagent ? " (sub)" : ""}`).join(", ");
  return html`<details class="models" open>
    <summary>Models: ${short || "none yet"}${calls ? ` · Agent ×${calls}` : ""}</summary>
    ${rows.length > 0 && html`<div class="table-wrap"><table>
      <thead><tr><th>role</th><th>model</th><th class="num">turns</th><th class="num">resp.</th><th class="num">in</th><th class="num">out</th></tr></thead>
      <tbody>${rows.map((r) => html`<tr>
        <td>${r.Subagent ? "subagent" : "main"}</td><td>${shortModel(r.Model)}</td>
        <td class="num">${r.Events}</td><td class="num">${r.Responses}</td>
        <td class="num">${fmtTokens(r.Input)}</td><td class="num">${fmtTokens(r.Output)}</td></tr>`)}</tbody>
    </table></div>`}
    <div class="muted">Agent tool calls: ${calls}</div>
    ${tokens.length > 0 && html`<div class="table-wrap"><table>
      <caption>Claude Code's count, by the name it asked for</caption>
      <thead><tr><th>name</th><th class="num">in</th><th class="num">cache r</th><th class="num">cache w</th><th class="num">out</th></tr></thead>
      <tbody>${tokens.map((t) => html`<tr><td>${t.Name}</td><td class="num">${fmtTokens(t.Input)}</td>
        <td class="num">${fmtTokens(t.CacheRead)}</td><td class="num">${fmtTokens(t.CacheWrite)}</td><td class="num">${fmtTokens(t.Out)}</td></tr>`)}</tbody>
    </table></div>`}
  </details>`;
}

/**
 * "Auto decisions: N allowed, M denied, K asked" and the policy switch (DESIGN §18.3).
 * A router session in default or acceptEdits has the switch too, off; turning it on
 * makes it a mado auto session from now on (§18.6).
 */
function AutoPanel({ auto, setAuto, madoAuto }) {
  return html`<section class="auto-panel">
    <h3>Auto decisions</h3>
    <div class="auto-counts"><span class="ok">${auto.allowed} allowed</span>, <span class="err">${auto.denied} denied</span>, <span class="warn">${auto.asked} asked</span></div>
    <label class="switch">
      <input type="checkbox" role="switch" checked=${auto.enabled} onChange=${(e) => setAuto(e.currentTarget.checked)} />
      <span>${auto.enabled ? "mado decides approvals" : "every approval asks you"}</span>
    </label>
    ${!madoAuto && html`<div class="muted small">turn on to let mado decide approvals from now on</div>`}
    <div class="muted small">${auto.classifier ? `rules, then the classifier ${auto.classifier}` : "rules only; the rest asks you"}</div>
  </section>`;
}

/**
 * A pending question in the panel (DESIGN §21.2): a button that closes the sheet and
 * brings the question card in the transcript into view, its first choice focused.
 */
function QuestionLink({ a, qs, onClose }) {
  const open = () => {
    onClose();
    const el = document.getElementById("q-" + a.request_id);
    if (!el) return;
    el.scrollIntoView({ behavior: "smooth", block: "start" });
    el.classList.add("flash");
    setTimeout(() => el.classList.remove("flash"), 1200);
    const first = el.querySelector("input");
    if (first) first.focus({ preventScroll: true });
  };
  const q = qs[0] || {};
  return html`<button type="button" class="q-link" onClick=${open}>
    <span class="q-link-head">${q.header ? html`<span class="q-chip">${q.header}</span>` : null}Question${qs.length > 1 ? ` (${qs.length})` : ""}</span>
    <span class="q-link-text">${q.question}</span>
  </button>`;
}

/**
 * The permission mode of a code session, changeable while it runs (DESIGN §25.4):
 * auto is mado's on a router profile and claude's own on a subscription one.
 */
function ModeSelect({ s, setMode }) {
  const modes = ["default", "acceptEdits", "auto"];
  const cur = modes.includes(s.permission_mode) ? s.permission_mode : "default";
  return html`<select class="mode-select" aria-label="Permission mode" value=${cur}
    onChange=${(e) => setMode(e.currentTarget.value)}>
    ${modes.map((m) => html`<option key=${m} value=${m}>${modeOptionLabel(m, s.profile_kind === "router")}</option>`)}
  </select>`;
}

/**
 * The subscription's usage windows from the latest rate_limit_event (DESIGN §26.3):
 * "5 h: 26 % · resets 11:30", "7 d: 53 % · resets Nov 3 09:00", and the auto-resume
 * switch.
 */
function LimitsPanel({ s, setAutoResume }) {
  const info = s.rate_limit || {};
  const w = info.unifiedWindows || {};
  const rows = [["five_hour", "5 h", false], ["seven_day", "7 d", true]]
    .filter(([k]) => w[k])
    .map(([k, label, date]) => {
      const x = w[k];
      const pct = typeof x.utilization === "number" ? `${Math.round(x.utilization * 100)} %` : "?";
      return html`<div key=${k}>${label}: ${pct}${x.resetsAt ? ` · resets ${fmtClock(x.resetsAt, date)}` : ""}</div>`;
    });
  if (!rows.length && info.resetsAt) {
    rows.push(html`<div>${info.status || "limit"}${info.resetsAt ? ` · resets ${fmtClock(info.resetsAt, true)}` : ""}</div>`);
  }
  return html`<section class="limits-panel">
    <h3>Limits</h3>
    ${rows}
    <label class="switch small"><input type="checkbox" checked=${!!s.auto_resume}
      onChange=${(e) => setAutoResume(e.currentTarget.checked)} /> <span>Resume automatically when the limit resets</span></label>
  </section>`;
}

function usageIn(u) {
  return (u.input || 0) + (u.cache_read || 0) + (u.cache_write || 0);
}

/**
 * @param {{s:any, detail:any, models:any, pending:any[], actions:any, onDecided:(a:any)=>void, onClose:()=>void,
 *   noise:any, currency:string}} p
 */
export function Panel({ s, detail, models, pending, actions, onDecided, onClose, noise, currency, file, restart }) {
  if (!s) return html`<aside class="panel"></aside>`;
  if (file && file.path) return html`<${FilePanel} s=${s} file=${file} />`;
  const st = baseState(s.state);
  const u = (detail && detail.usage) || {};
  const mu = (detail && detail.month_usage) || {};
  const files = (detail && detail.files) || [];
  const hubRun = s.kind === "chat" || (s.kind === "digest" && !s.machine_id); // run by the hub's chat runner
  const canDelete = hubRun || st === "exited" || st === "detached";
  return html`<aside class="panel" aria-label="session details">
    <div class="panel-head"><strong>Details</strong>
      <button type="button" class="icon-btn" aria-label="close details" onClick=${onClose}>×</button></div>
    <section>
      <${Row} k="state">${s.state}${s.last_error ? `: ${s.last_error}` : ""}<//>
      ${s.kind === "digest" && s.machine_id
        ? html`<${Row} k="machine">${s.machine}${s.online ? "" : " (offline)"}<//>
            <${Row} k="executor">claude (job session)<//>
            <${Row} k="main model">${s.main_model}<//>
            <${Row} k="tools">${(detail && detail.tools && detail.tools.length) ? detail.tools.join(", ") : "none"}<//>`
        : s.kind === "code"
        ? html`<${Row} k="machine">${s.machine}${s.online ? "" : " (offline)"}<//>
            <${Row} k="cwd"><code>${s.cwd}</code><//>
            <${Row} k="profile">${s.profile}<//>
            <${Row} k="main model">${s.main_model}<//>
            <${Row} k="permissions"><${ModeSelect} s=${s} setMode=${actions.setMode} /><//>
            ${restart && html`<button type="button" onClick=${actions.restart}
              title="Stop claude and resume the conversation in the new mode">Restart to apply</button>`}
            ${s.resumed_from && html`<div class="muted small">Continued from <a href=${"/s/" + s.resumed_from}>an earlier session</a></div>`}
            <a class="nav-inline" href=${`/new?kind=code&machine=${encodeURIComponent(s.machine_id)}&cwd=${encodeURIComponent(s.cwd)}`}
              title="Start another session (same project preselected)">New session here</a>
            ${s.resumable && html`<button type="button" onClick=${actions.resume}
              title="Continue this conversation in a new session (new shim, current profile settings)">Resume</button>`}`
        : html`<${Row} k="model">${s.model}<//>
            <${Row} k="tools">${(detail && detail.tools && detail.tools.length) ? detail.tools.join(", ") : "none"}<//>
            ${s.system && html`<details class="more"><summary>system prompt</summary><pre>${s.system}</pre></details>`}`}
    </section>
    ${s.concierge && html`<section>
      <h3>Started sessions</h3>
      ${!(detail && detail.spawned && detail.spawned.length) ? html`<div class="muted">none yet</div>`
        : html`<ul class="files">${detail.spawned.map((x) => html`<li key=${x.id}>
            <a href=${"/s/" + x.id}>${x.title || x.id}</a><span class="muted"> ${x.machine} · ${x.state}</span></li>`)}</ul>`}
    </section>`}
    ${s.kind === "code" && detail && detail.auto && html`<${AutoPanel} auto=${detail.auto} setAuto=${actions.setAuto} madoAuto=${!!s.mado_auto} />`}
    ${s.kind === "code" && s.profile_kind === "subscription" && s.rate_limit &&
      html`<${LimitsPanel} s=${s} setAutoResume=${actions.setAutoResume} />`}
    ${!hubRun && html`<section><${ModelsPanel} stats=${models} /></section>`}
    ${s.kind === "code" && html`<section>
      <h3>Pending approvals${pending.length ? ` (${pending.length})` : ""}</h3>
      ${pending.length === 0 ? html`<div class="muted">none</div>`
        : pending.map((a) => {
          const qs = questionsOf(a);
          return qs ? html`<${QuestionLink} key=${a.request_id} a=${a} qs=${qs} onClose=${onClose} />`
            : html`<${ApprovalCard} key=${a.request_id} a=${a} sessionID=${s.id} onDecided=${onDecided} />`;
        })}
    </section>`}
    ${hubRun && html`<section>
      <h3>Usage</h3>
      <${Row} k="this session">${fmtTokens(usageIn(u))} in · ${fmtTokens(u.output)} out<//>
      <${Row} k="cost">${fmtMoney(u.cost, currency)}<//>
      <${Row} k="this month">${fmtTokens(usageIn(mu))} in · ${fmtTokens(mu.output)} out · ${fmtMoney(mu.cost, currency)}<//>
    </section>
    <section>
      <h3>Attachments</h3>
      ${files.length === 0 ? html`<div class="muted">none</div>`
        : html`<ul class="files">${files.map((f) => html`<li key=${f.id}>
            <a href=${f.url} target="_blank" rel="noopener noreferrer">${f.name}</a>
            <span class="muted"> ${fmtBytes(f.size)}</span></li>`)}</ul>`}
    </section>`}
    <section class="noise-panel">${noise}</section>
    <section class="actions">
      <button type="button" class="secondary" onClick=${actions.rename}>Rename</button>
      ${s.running && html`<button type="button" class="secondary" onClick=${actions.interrupt} title="End the current turn">Interrupt</button>`}
      ${s.kind === "code" && st !== "exited" && st !== "detached" &&
        html`<button type="button" class="secondary" onClick=${actions.stop} title="End the turn, then let claude exit">Stop</button>`}
      <button type="button" class="deny" disabled=${!canDelete} onClick=${actions.remove}
        title=${canDelete ? "Delete the session, its events and files" : "Stop the session first"}>Delete</button>
    </section>
  </aside>`;
}

/**
 * A path linked inside a Markdown file: absolute and "~" paths as they are, a relative
 * one next to the file (as a Markdown link would be), "." and ".." folded.
 */
export function besideFile(/** @type {string} */ file, /** @type {string} */ p) {
  if (p.startsWith("/") || p.startsWith("~")) return p;
  const out = file.split("/").slice(0, -1);
  for (const seg of p.split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") { if (out.length > 1) out.pop(); } else out.push(seg);
  }
  return out.join("/") || "/";
}

/**
 * The file viewer (DESIGN §27.3): the right panel in "File" mode. file.path is the path
 * as written in the transcript; file.back returns to the previous file or the normal
 * panel, file.close to the normal panel, file.open opens a path linked in a Markdown file.
 * @param {{s:any, file:{path:string, depth:number, back:()=>void, close:()=>void, open:(p:string)=>void}}} p
 */
function FilePanel({ s, file }) {
  const [st, setSt] = useState(/** @type {{loading:boolean, data?:any, err?:any}} */ ({ loading: true }));
  useEffect(() => {
    let live = true;
    setSt({ loading: true });
    get(`/api/sessions/${encodeURIComponent(s.id)}/file?path=${encodeURIComponent(file.path)}`).then(
      (data) => { if (live) setSt({ loading: false, data }); },
      (err) => { if (live) setSt({ loading: false, err }); });
    return () => { live = false; };
  }, [s.id, file.path]);
  const dl = `/api/sessions/${encodeURIComponent(s.id)}/file/download?path=${encodeURIComponent(file.path)}`;
  const d = st.data;
  const relative = !file.path.startsWith("/") && !file.path.startsWith("~");
  const download = html`<a class="button secondary" href=${dl} download>Download</a>`;
  let body;
  if (st.loading) body = html`<div class="muted">Loading…</div>`;
  else if (st.err && st.err.status === 413) {
    body = html`<div class="notice">${st.err.message}<div class="file-dl">${download}</div></div>`;
  } else if (st.err) body = html`<div class="notice bad">${st.err.message}</div>`;
  else if (d.kind === "markdown") body = html`<${Markdown} html=${d.html} onPath=${(p) => file.open(besideFile(d.path, p))} />`;
  else if (d.kind === "text") body = html`<pre class="file-text">${d.text}</pre>`;
  else body = html`<div class="notice">Not a text file${d.mime ? ` (${d.mime})` : ""}; it can only be downloaded.
    <div class="file-dl">${download}</div></div>`;
  return html`<aside class="panel file-panel" aria-label="file viewer">
    <div class="file-head">
      <div class="file-bar">
        <button type="button" class="secondary" onClick=${file.back}
          title=${file.depth > 1 ? "The previous file" : "Back to the session details"}>Back</button>
        <strong>File</strong>
        <span class="spacer"></span>
        ${!st.err && html`<a href=${dl} download>Download</a>`}
        <button type="button" class="icon-btn" aria-label="close the file" onClick=${file.close}>×</button>
      </div>
      <div class="file-path"><code>${d ? d.path : file.path}</code></div>
      ${d && html`<div class="muted small">${fmtBytes(d.size)}${d.modified ? ` · modified ${new Date(d.modified).toLocaleString()}` : ""}</div>`}
      ${relative && html`<div class="muted small">"${file.path}" is resolved on ${s.machine} relative to <code>${s.cwd}</code></div>`}
    </div>
    <div class="file-body" key=${file.path}>${body}</div>
  </aside>`;
}
