// /new (and /): three tabs. Chat: model and an optional system prompt (and first
// message). Code: machine, a project from the machine's reported roots, profile and
// main model (the profile's alias and role tables read-only, DESIGN §15), permission
// mode. Plan: the concierge (DESIGN §20), an optional first message. On success the
// app navigates to /s/{id}.
import { html, useState, useEffect } from "../lib.js";
import { api, get } from "../api.js";
import { toast } from "../components/toast.js";
import { TopBar } from "./session.js";
import { cx } from "../util.js";

const modeHelp = {
  default: "ask me",
  acceptEdits: "file edits without asking",
  auto: "claude decides: Claude Code's own auto mode",
};

/** The mode's option text; auto on a router profile is mado auto (DESIGN §18.1, §25.1). */
function modeLabel(mode, profile) {
  if (mode === "auto" && profile && profile.kind === "router") return "auto (mado decides): edits pass, mado's rules and classifier decide the rest";
  return modeHelp[mode] ? `${mode} (${modeHelp[mode]})` : mode;
}

function ChatForm({ app }) {
  const [models, setModels] = useState(null);
  const [model, setModel] = useState("");
  const [system, setSystem] = useState("");
  const [first, setFirst] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    get("/api/chat/models").then((r) => {
      setModels(r.models || []);
      const d = (r.models || []).find((m) => m.default) || (r.models || [])[0];
      if (d) setModel(d.name);
    }, (e) => toast(e.message));
  }, []);
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      const r = await api("POST", "/api/sessions", { kind: "chat", model, system });
      if (first.trim()) {
        try { await api("POST", `/api/sessions/${r.id}/send`, { text: first }); } catch (err) { toast(err.message); }
      }
      app.refreshSessions();
      app.navigate("/s/" + r.id);
    } catch (err) {
      toast(err.message);
      setBusy(false);
    }
  };
  if (!models) return html`<div class="muted">Loading…</div>`;
  if (!models.length) return html`<p class="muted">No chat models are configured on this hub.</p>`;
  const m = models.find((x) => x.name === model);
  return html`<form class="stack" onSubmit=${submit} autocomplete="off">
    <label>Model
      <select value=${model} onChange=${(e) => setModel(e.currentTarget.value)}>
        ${models.map((x) => html`<option key=${x.name} value=${x.name}>${x.label || x.name}${x.default ? " (default)" : ""}</option>`)}
      </select>
    </label>
    ${m && html`<p class="muted hint">${Math.round(m.context_window / 1000)}k context · ${m.vision ? "sees images" : "text only"}${m.documents ? " · native PDFs" : ""}</p>`}
    <label>System prompt (optional)
      <textarea rows="3" maxlength="4000" value=${system} onInput=${(e) => setSystem(e.currentTarget.value)}
        placeholder="e.g. Answer briefly."></textarea>
    </label>
    <label>First message (optional)
      <textarea rows="3" value=${first} onInput=${(e) => setFirst(e.currentTarget.value)}></textarea>
    </label>
    <div><button type="submit" disabled=${busy || !model}>Start chat</button></div>
  </form>`;
}

function CodeForm({ app, preset }) {
  const [machines, setMachines] = useState(null);
  const [modes, setModes] = useState([]);
  const [profiles, setProfiles] = useState([]);
  const [machine, setMachine] = useState("");
  const [cwd, setCwd] = useState("");
  const [profile, setProfile] = useState("subscription");
  const [main, setMain] = useState("");
  const [mode, setMode] = useState("auto"); // DESIGN §25.2
  const [title, setTitle] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    get("/api/machines").then((r) => {
      setMachines(r.machines || []);
      setModes(r.permission_modes || ["default"]);
      const ms = r.machines || [];
      // The preset (from "New session here"), else the first online machine with projects, else the first.
      const pre = ms.find((m) => m.id === preset.machine);
      const pick = pre || ms.find((m) => m.online && m.projects.length) || ms[0];
      if (pick) {
        setMachine(pick.id);
        setCwd(pre && pick.projects.includes(preset.cwd) ? preset.cwd : pick.projects[0] || "");
      }
    }, (e) => toast(e.message));
    get("/api/profiles").then((r) => {
      setProfiles(r.profiles || []);
      if (r.profiles && r.profiles.length && !r.profiles.some((p) => p.name === "subscription")) setProfile(r.profiles[0].name);
    }, (e) => toast(e.message));
  }, []);
  const m = (machines || []).find((x) => x.id === machine);
  const p = profiles.find((x) => x.name === profile);
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      const r = await api("POST", "/api/sessions", { kind: "code", machine, cwd, profile, main_model: main, permission_mode: mode, title });
      app.refreshSessions();
      app.navigate("/s/" + r.id);
    } catch (err) {
      toast(err.message);
      setBusy(false);
    }
  };
  if (!machines) return html`<div class="muted">Loading…</div>`;
  if (!machines.length) {
    return html`<p class="muted">No machines enrolled. An admin runs <code>${"mado hub enroll <name>"}</code> on the hub
      ${app.me.is_admin && html` or uses <a href="/admin/enroll">Enroll machine</a>`}.</p>`;
  }
  return html`<form class="stack" onSubmit=${submit} autocomplete="off">
    <label>Machine
      <select value=${machine} onChange=${(e) => {
        const id = e.currentTarget.value;
        setMachine(id);
        const mm = machines.find((x) => x.id === id);
        setCwd(mm && mm.projects.length ? mm.projects[0] : "");
      }}>
        ${machines.map((x) => html`<option key=${x.id} value=${x.id} disabled=${!x.online}>${x.name}${x.online ? "" : " (offline)"}</option>`)}
      </select>
    </label>
    ${m && !m.online && html`<p class="hint warn-text">${m.name} is offline; a session needs a connected worker.</p>`}
    ${m && html`<p class="muted hint">${m.claude_version ? m.claude_version + " · " : ""}${m.projects.length} projects${m.last_seen ? " · seen " + m.last_seen : ""}</p>`}
    <label>Project
      <select value=${cwd} onChange=${(e) => setCwd(e.currentTarget.value)} disabled=${!m || !m.projects.length}>
        ${m && m.projects.map((x) => html`<option key=${x} value=${x}>${x}</option>`)}
      </select>
    </label>
    <label>Profile
      <select value=${profile} onChange=${(e) => { setProfile(e.currentTarget.value); setMain(""); }}>
        ${profiles.map((x) => html`<option key=${x.name} value=${x.name}>${x.name}</option>`)}
      </select>
    </label>
    <label>Main model
      <select value=${main} onChange=${(e) => setMain(e.currentTarget.value)}>
        <option value="">profile main (default)</option>
        ${p && p.choices.map((c) => html`<option key=${c} value=${c}>${c}${c === p.main ? " (main)" : ""}</option>`)}
      </select>
    </label>
    ${p && html`<${ProfileInfo} p=${p} />`}
    <label>Permission mode
      <select value=${mode} onChange=${(e) => setMode(e.currentTarget.value)}>
        ${modes.map((x) => html`<option key=${x} value=${x}>${modeLabel(x, p)}</option>`)}
      </select>
    </label>
    <label>Title (optional)<input value=${title} maxlength="120" onInput=${(e) => setTitle(e.currentTarget.value)} /></label>
    <div><button type="submit" disabled=${busy || !m || !m.online || !cwd}>Start session</button></div>
    <p class="muted hint">In a code session, <code>${'/new <machine> "<project>" [profile] [mode]'}</code> starts another one.</p>
  </form>`;
}

function PlanForm({ app }) {
  const [first, setFirst] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      const r = await api("POST", "/api/sessions", { kind: "chat", concierge: true });
      if (first.trim()) {
        try { await api("POST", `/api/sessions/${r.id}/send`, { text: first }); } catch (err) { toast(err.message); }
      }
      app.refreshSessions();
      app.navigate("/s/" + r.id);
    } catch (err) {
      toast(err.message);
      setBusy(false);
    }
  };
  return html`<form class="stack" onSubmit=${submit} autocomplete="off">
    <p class="muted hint">Tell mado what you want to start. It looks at your machines and projects, then proposes a plan:
      directories to create (mkdir, git init, clone, worktree) and the sessions to start, with their first prompt.
      Nothing is written before you approve the plan; the design discussion then happens in the new session.</p>
    <label>First message (optional)
      <textarea rows="4" value=${first} onInput=${(e) => setFirst(e.currentTarget.value)}
        placeholder="e.g. A worktree of tt-metal on gpu1 to try a faster softmax"></textarea>
    </label>
    <div><button type="submit" disabled=${busy}>Start planning</button></div>
    <p class="muted hint">In any session, <code>${"/new plan"}</code> opens one too.</p>
  </form>`;
}

function ProfileInfo({ p }) {
  const table = (caption, rows, first) => html`<div class="table-wrap"><table class="aliases">
    <caption>${caption}</caption>
    <thead><tr><th>${first}</th><th>alias</th><th>upstream</th><th>model</th></tr></thead>
    <tbody>${rows.map((r) => html`<tr key=${r.Name}><td>${r.Name}</td><td>${r.Alias}</td><td>${r.Upstream || "Anthropic"}</td><td>${r.Model}</td></tr>`)}</tbody>
  </table></div>`;
  const enforce = { edits: " (the main model may not edit files; subagents may)", all: " (the main model may not edit files or run commands)" };
  return html`<details class="profile-info">
    <summary>${p.name}: models and role split</summary>
    ${table(p.name + ": main model and aliases", p.aliases || [], "name")}
    ${p.roles_on
      ? html`${table("role split: the main model delegates through the Agent tool", p.roles || [], "agent")}
          <p class="muted">enforce: ${p.enforce}${enforce[p.enforce] || " (prompt only)"}</p>`
      : html`<p class="muted">Role split off for this profile: one model does everything.</p>`}
  </details>`;
}

const headings = { chat: "What's on your mind?", code: "Start claude on a machine", plan: "What do you want to start?" };

export function NewView({ app, kind, preset }) {
  const tab = kind === "plan" && app.me.concierge ? "plan" : kind === "code" || !app.me.chat_enabled ? "code" : "chat";
  const go = (k) => app.navigate("/new?kind=" + k, true);
  return html`<div class="main">
    <${TopBar} app=${app} title="New session" />
    <div class="scroller"><div class="page-body">
      <h2 class="hello">${headings[tab]}</h2>
      <div class="tabs" role="tablist">
        ${app.me.chat_enabled && html`<button type="button" role="tab" aria-selected=${tab === "chat"} class=${cx("tab", tab === "chat" && "active")} onClick=${() => go("chat")}>Chat</button>`}
        <button type="button" role="tab" aria-selected=${tab === "code"} class=${cx("tab", tab === "code" && "active")} onClick=${() => go("code")}>Code</button>
        ${app.me.concierge && html`<button type="button" role="tab" aria-selected=${tab === "plan"} class=${cx("tab", tab === "plan" && "active")} onClick=${() => go("plan")}>Plan</button>`}
      </div>
      ${tab === "chat" ? html`<${ChatForm} app=${app} />` : tab === "plan" ? html`<${PlanForm} app=${app} />` : html`<${CodeForm} app=${app} preset=${preset} />`}
    </div></div>
  </div>`;
}
