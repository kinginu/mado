// The session transcript: blocks from GET /api/sessions/{id}/blocks plus live SSE
// blocks, grouped by buildTree. Assistant and user text is goldmark HTML from the hub
// (sanitized there, DESIGN §7); every other field is text and escaped by Preact.

import { html, useEffect, useRef, useState, useMemo, useCallback, createContext, useContext } from "../lib.js";
import { buildTree, chunkSummary } from "./chunks.js";
import { api } from "../api.js";
import { fmtTokens, cx } from "../util.js";
import { toast } from "./toast.js";

/** Copies text; the Clipboard API needs a secure context, so fall back to a selection. */
async function copyText(/** @type {string} */ text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (e) {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.className = "offscreen";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    ta.remove();
    return ok;
  }
}

/**
 * The file viewer's opener (DESIGN §27.3): a code session provides (path) => void and
 * the hub's fpath links in its Markdown open the file; elsewhere they stay plain text.
 */
export const PathContext = createContext(/** @type {((p: string) => void) | null} */ (null));

/**
 * Server-rendered Markdown, with a copy button on each code block. onPath (or the
 * PathContext) opens a clicked file path.
 */
export function Markdown({ html: content, onPath }) {
  const ref = useRef(null);
  const ctxPath = useContext(PathContext);
  const open = onPath || ctxPath;
  const onClick = (/** @type {MouseEvent} */ e) => {
    const a = /** @type {HTMLElement} */ (e.target).closest && /** @type {HTMLElement} */ (e.target).closest("a.fpath");
    if (!a) return;
    e.preventDefault();
    if (open) open(a.getAttribute("data-path") || "");
  };
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.querySelectorAll("pre").forEach((pre) => {
      if (pre.parentElement && pre.parentElement.classList.contains("code-wrap")) return;
      const wrap = document.createElement("div");
      wrap.className = "code-wrap";
      pre.replaceWith(wrap);
      wrap.appendChild(pre);
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "copy";
      btn.textContent = "Copy";
      btn.addEventListener("click", async () => {
        const ok = await copyText(pre.innerText);
        btn.textContent = ok ? "Copied" : "Copy failed";
        setTimeout(() => { btn.textContent = "Copy"; }, 1500);
      });
      wrap.appendChild(btn);
    });
  }, [content]);
  return html`<div class=${cx("text md", open && "paths")} ref=${ref} onClick=${onClick} dangerouslySetInnerHTML=${{ __html: content || "" }}></div>`;
}

function Details({ summary, text }) {
  if (!text) return null;
  return html`<details class="more"><summary>${summary}</summary><pre>${text}</pre></details>`;
}

/** web_search results ("1. title\n   url\n   snippet") as a list of links. */
export function parseSearchResults(/** @type {string} */ text) {
  const out = [];
  const re = /^(\d+)\. (.*)\n {3}(https?:\/\/\S+)(?:\n {3}(?!\d+\. )(.*))?$/gm;
  let m;
  while ((m = re.exec(text)) !== null) out.push({ title: m[2], url: m[3], snippet: m[4] || "" });
  return out;
}

function isSearch(/** @type {string|undefined} */ tool) {
  return tool === "web_search" || tool === "mcp__mado__web_search";
}

function SearchResults({ items }) {
  return html`<ol class="search-results">${items.map((r, i) => html`<li key=${i}>
    <a href=${r.url} target="_blank" rel="noopener noreferrer nofollow">${r.title || r.url}</a>
    <div class="url muted">${r.url}</div>
    ${r.snippet && html`<div class="snippet">${r.snippet}</div>`}
  </li>`)}</ol>`;
}

function BlockView({ node }) {
  const b = node.b;
  const cls = cx("blk", "k-" + b.kind, b.is_error && "err", b.subagent && "sub", b.noise && "noise");
  switch (b.kind) {
    case "user":
      return html`<div class=${cls}><${Markdown} html=${b.html} /></div>`;
    case "assistant":
      return html`<div class=${cls}><${Markdown} html=${b.html} /></div>`;
    case "tool_use":
      return html`<div class=${cls}>
        <div class="tool"><span class="name">${b.label}</span> <code>${b.text}</code></div>
        <${Details} summary="input" text=${b.detail} />
      </div>`;
    case "tool_result": {
      const hits = !b.is_error && isSearch(node.tool) ? parseSearchResults(b.detail || b.text) : [];
      if (hits.length) {
        return html`<div class=${cls}><${SearchResults} items=${hits} />
          <${Details} summary="as text" text=${b.detail || b.text} /></div>`;
      }
      return html`<div class=${cls}>
        <pre class="result">${b.text}</pre>
        <${Details} summary="full result" text=${b.detail} />
      </div>`;
    }
    case "file": {
      const url = "/files/" + encodeURIComponent(b.file_id || "");
      const img = (b.mime || "").startsWith("image/");
      if (b.origin === "generated") {
        return html`<div class=${cx(cls, "generated")}><figure class="gen">
          <a href=${url} target="_blank" rel="noopener noreferrer"><img src=${url} alt="generated image" class="gen-img" /></a>
          <figcaption class="muted"><a href=${url} download=${b.label}>Download</a> · ${b.label}</figcaption>
        </figure></div>`;
      }
      return html`<div class=${cls}><div class="file">
        ${img && html`<a href=${url} target="_blank" rel="noopener noreferrer"><img src=${url} alt=${b.label} class="thumb" /></a>`}
        <a href=${url} target="_blank" rel="noopener noreferrer">${b.label}</a> <span class="muted">${b.mime}</span>
      </div></div>`;
    }
    case "note":
      return html`<div class=${cls}><div class="note-head muted">mado</div><${Markdown} html=${b.html} /></div>`;
    case "raw":
      return html`<div class=${cls}><${Details} summary=${b.label} text=${b.detail} /></div>`;
    case "thinking":
      return html`<div class=${cls}><details class="more"><summary>thinking</summary><pre>${b.text}</pre></details></div>`;
    default:
      return html`<div class=${cls}>
        <div class="text muted">${b.text}${node.thinking ? ` · thinking ${fmtTokens(node.thinking)} tokens` : ""}</div>
        <${Details} summary="details" text=${b.detail} />
      </div>`;
  }
}

/** decidedByPolicy: the hub's auto mode answered (DESIGN §18.3), not a viewer. */
export function decidedByPolicy(a) {
  return a.state !== "pending" && !!a.auto && a.auto !== "ask";
}

/**
 * The one-line heading of an automatic decision: "auto-allowed by mado: git status",
 * "auto-denied by the classifier: <reason>".
 */
export function autoHeading(a) {
  const verb = a.decision === "deny" ? "auto-denied" : "auto-allowed";
  if (a.decided_by === "classifier") return `${verb} by the classifier${a.reason ? ": " + a.reason : ""}`;
  const what = a.decided_by && a.decided_by.startsWith("rule:") ? a.decided_by.slice(5) : a.reason;
  return `${verb} by mado${what ? ": " + what : ""}`;
}

/** A decided card of mado auto mode: one collapsed line, the call inside. */
function AutoCard({ a }) {
  const denied = a.decision === "deny";
  return html`<details class=${cx("blk", "approval", "decided", "auto", denied ? "auto-deny" : "auto-allow noise")}>
    <summary class="auto-head">${autoHeading(a)}</summary>
    <div class="tool"><span class="name">${a.tool_name}</span> <code>${a.summary}</code></div>
    <${Details} summary="input" text=${a.detail} />
  </details>`;
}

/** questionsOf: the questions of an AskUserQuestion approval (DESIGN §21.2). */
export function questionsOf(a) {
  return a && a.tool_name === "AskUserQuestion" && Array.isArray(a.questions) ? a.questions : null;
}

const OTHER = "\u0000other";

/**
 * answerOf is the answer text of one question from its form state: a single choice is
 * the option label, a multiSelect the chosen labels (in option order, then "Other")
 * joined with ", " as Claude Code joins them, "Other" the typed text.
 */
export function answerOf(q, st) {
  const other = (st.other || "").trim();
  const opts = q.options || [];
  if (!opts.length) return other;
  if (q.multiSelect) {
    const parts = opts.map((o) => o.label).filter((l) => st.multi && st.multi[l]);
    if (st.otherOn && other) parts.push(other);
    return parts.join(", ");
  }
  if (st.sel === OTHER) return other;
  return st.sel || "";
}

/** One question of the card: header chip, text, options, preview, Other, notes. */
function QuestionField({ q, i, rid, st, set, disabled }) {
  const opts = q.options || [];
  const name = `q-${rid}-${i}`;
  const [focus, setFocus] = useState(null);
  const shown = opts.find((o) => o.label === focus && o.preview) ||
    (!q.multiSelect && opts.find((o) => o.label === st.sel && o.preview)) || null;
  const hasPreview = opts.some((o) => o.preview);
  const type = q.multiSelect ? "checkbox" : "radio";
  const otherChecked = q.multiSelect ? !!st.otherOn : st.sel === OTHER;
  const pick = (label, on) => {
    if (q.multiSelect) set({ multi: { ...(st.multi || {}), [label]: on } });
    else set({ sel: label });
  };
  return html`<fieldset class="q-field" disabled=${disabled}>
    <legend class="q-text">${q.header && html`<span class="q-chip">${q.header}</span>`}<span>${q.question}</span>
      ${q.multiSelect && html`<span class="muted small"> (choose any)</span>`}</legend>
    ${opts.length > 0 && html`<div class="q-options" role=${q.multiSelect ? "group" : "radiogroup"}>
      ${opts.map((o) => html`<label key=${o.label} class=${cx("q-opt", (q.multiSelect ? st.multi && st.multi[o.label] : st.sel === o.label) && "on")}
          onMouseEnter=${() => o.preview && setFocus(o.label)} onMouseLeave=${() => setFocus(null)}>
        <input type=${type} name=${name} checked=${q.multiSelect ? !!(st.multi && st.multi[o.label]) : st.sel === o.label}
          onFocus=${() => o.preview && setFocus(o.label)} onBlur=${() => setFocus(null)}
          onChange=${(e) => pick(o.label, e.currentTarget.checked)} />
        <span class="q-opt-body"><span class="q-label">${o.label}</span>
          ${o.description && html`<span class="q-desc">${o.description}</span>`}</span>
      </label>`)}
      <label class=${cx("q-opt", "q-other", otherChecked && "on")}>
        <input type=${type} name=${name} checked=${otherChecked}
          onChange=${(e) => (q.multiSelect ? set({ otherOn: e.currentTarget.checked }) : set({ sel: OTHER }))} />
        <span class="q-opt-body"><span class="q-label">Other</span>
          <input type="text" class="q-other-text" placeholder="Type your own answer" value=${st.other || ""} maxlength="2000"
            aria-label=${"Other answer to: " + q.question}
            onFocus=${() => (q.multiSelect ? set({ otherOn: true }) : set({ sel: OTHER }))}
            onInput=${(e) => set({ other: e.currentTarget.value })} /></span>
      </label>
    </div>`}
    ${!opts.length && html`<input type=${q.kind === "number" ? "number" : "text"} class="q-free" value=${st.other || ""} maxlength="2000"
      min=${q.min} max=${q.max} step=${q.step} placeholder=${q.placeholder || "Your answer"} aria-label=${q.question}
      onInput=${(e) => set({ other: e.currentTarget.value })} />`}
    ${hasPreview && html`<pre class=${cx("q-preview", !shown && "empty")} aria-live="polite">${shown ? shown.preview : "Select or focus an option to see its preview."}</pre>`}
    <details class="more q-notes" open=${!!st.notesOpen} onToggle=${(e) => set({ notesOpen: e.currentTarget.open })}>
      <summary>add notes</summary>
      <textarea rows="2" maxlength="2000" placeholder="Anything claude should know about this answer" value=${st.notes || ""}
        aria-label=${"Notes on: " + q.question} onInput=${(e) => set({ notes: e.currentTarget.value })}></textarea>
    </details>
  </fieldset>`;
}

/**
 * The question card of an AskUserQuestion (DESIGN §21.2): Answer sends allow with the
 * answers (and notes), Dismiss sends deny; there is no "allow for session".
 */
export function QuestionCard({ a, qs, sessionID, onDecided }) {
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState(() => qs.map(() => ({})));
  const pending = a.state === "pending";
  if (!pending) {
    const answers = a.answers || {};
    const notes = a.notes || {};
    const what = a.decision === "deny" ? "dismissed" : a.decision === "expired" ? "expired: claude stopped waiting" : a.decision;
    return html`<div class="blk approval question decided" id=${"q-" + a.request_id}>
      <div class="q-title">${a.answers ? "Answered" : "Question"} <span class="muted">· ${a.answers ? qs.length + (qs.length === 1 ? " question" : " questions") : what}</span></div>
      ${a.answers
        ? html`<dl class="q-answers">${qs.map((q, i) => html`<div key=${i}>
            <dt>${q.header && html`<span class="q-chip">${q.header}</span>`}${q.question}</dt>
            <dd>${answers[q.question] || html`<span class="muted">not answered</span>`}
              ${notes[q.question] && html`<div class="q-note muted">notes: ${notes[q.question]}</div>`}</dd></div>`)}</dl>`
        : html`<ul class="q-asked">${qs.map((q, i) => html`<li key=${i}>${q.question}</li>`)}</ul>`}
    </div>`;
  }
  const set = (i) => (patch) => setForm((f) => f.map((x, j) => (j === i ? { ...x, ...patch } : x)));
  const answers = qs.map((q, i) => answerOf(q, form[i] || {}));
  const ready = answers.every((x) => x !== "");
  const send = async (decision) => {
    setBusy(true);
    try {
      const body = { request_id: a.request_id, decision };
      if (decision === "allow") {
        body.answers = {};
        const notes = {};
        qs.forEach((q, i) => {
          body.answers[q.question] = answers[i];
          const n = ((form[i] || {}).notes || "").trim();
          if (n) notes[q.question] = n;
        });
        if (Object.keys(notes).length) body.notes = notes;
      }
      onDecided(await api("POST", `/api/sessions/${sessionID}/approve`, body));
    } catch (e) {
      toast(e.message);
    } finally {
      setBusy(false);
    }
  };
  return html`<form class=${cx("blk", "approval", "question", a.auto === "ask" && "asked")} id=${"q-" + a.request_id}
      onSubmit=${(e) => { e.preventDefault(); if (ready && !busy) send("allow"); }}>
    <div class="q-title">claude asks you${qs.length > 1 ? ` ${qs.length} questions` : ""}</div>
    ${qs.map((q, i) => html`<${QuestionField} key=${i} q=${q} i=${i} rid=${a.request_id} st=${form[i] || {}} set=${set(i)} disabled=${busy} />`)}
    <div class="buttons q-buttons">
      <button type="submit" disabled=${busy || !ready}>Answer</button>
      <button type="button" class="link" disabled=${busy} onClick=${() => send("deny")}>Dismiss</button>
      ${!ready && html`<span class="muted small">answer every question to send</span>`}
    </div>
  </form>`;
}

/** An approval card: Allow / Allow for session / Deny (DESIGN §7). */
export function ApprovalCard({ a, sessionID, onDecided }) {
  const [busy, setBusy] = useState(false);
  const qs = questionsOf(a);
  if (qs) return html`<${QuestionCard} a=${a} qs=${qs} sessionID=${sessionID} onDecided=${onDecided} />`;
  if (decidedByPolicy(a)) return html`<${AutoCard} a=${a} />`;
  const decide = async (decision) => {
    setBusy(true);
    try {
      const v = await api("POST", `/api/sessions/${sessionID}/approve`, { request_id: a.request_id, decision });
      onDecided(v);
    } catch (e) {
      toast(e.message);
    } finally {
      setBusy(false);
    }
  };
  const pending = a.state === "pending";
  return html`<div class=${cx("blk", "approval", !pending && "decided", a.auto === "ask" && "asked")}>
    <div class="tool"><span class="name">Allow ${a.tool_name}?</span> <code>${a.summary}</code></div>
    ${a.auto === "ask" && html`<div class="auto-reason">mado asks you: ${a.reason}</div>`}
    <${Details} summary="input" text=${a.detail} />
    ${pending
      ? html`<div class="buttons">
          <button type="button" disabled=${busy} onClick=${() => decide("allow")}>Allow</button>
          <button type="button" class="secondary" disabled=${busy} onClick=${() => decide("allow_session")}>Allow for session</button>
          <button type="button" class="deny" disabled=${busy} onClick=${() => decide("deny")}>Deny</button>
        </div>`
      : html`<div class="muted">${a.decision}</div>`}
  </div>`;
}

const stepMark = { pending: "○", running: "◌", done: "✓", failed: "✗", skipped: "–" };
const planStateText = { proposed: "waiting for you", running: "running", done: "done", failed: "failed", rejected: "rejected" };

/**
 * The concierge's plan card (DESIGN §20.3): every step with its state, the first
 * prompts expandable, Approve / Reject while proposed, links to the started sessions.
 */
export function PlanCard({ p, sessionID, onPlan }) {
  const [busy, setBusy] = useState(false);
  if (!p) return null;
  const plan = p.plan || {};
  const prep = (plan.prepare || []).length;
  const decide = async (decision) => {
    setBusy(true);
    try {
      onPlan(await api("POST", `/api/sessions/${sessionID}/plans/${p.id}`, { decision }));
    } catch (e) {
      toast(e.message);
    } finally {
      setBusy(false);
    }
  };
  return html`<div class=${cx("blk", "plan-card", "p-" + p.state)}>
    <div class="plan-head"><span class="plan-title">Plan</span>
      <span class=${"state s-plan-" + p.state}>${planStateText[p.state] || p.state}</span></div>
    <div class="plan-summary">${plan.summary}</div>
    <ol class="plan-steps">${(p.steps || []).map((st, i) => {
      const sess = st.kind === "session" ? (plan.sessions || [])[i - prep] : null;
      return html`<li key=${i} class=${"st-" + st.state}>
        <span class="mark" aria-label=${st.state}>${stepMark[st.state] || "○"}</span>
        <div class="step-body">
          <div class="step-label">${st.label}</div>
          ${st.session_id && html`<a class="step-link" href=${"/s/" + st.session_id}>${st.state === "done" ? "session started → open" : "open session"}</a>`}
          ${sess && sess.first_prompt && html`<details class="more"><summary>first prompt</summary><pre>${sess.first_prompt}</pre></details>`}
          ${st.output && (st.state === "failed"
            ? html`<pre class="step-err">${st.output}</pre>`
            : html`<details class="more"><summary>output</summary><pre>${st.output}</pre></details>`)}
        </div>
      </li>`;
    })}</ol>
    ${p.error && html`<div class="plan-error">${p.error}</div>`}
    ${p.state === "proposed" && html`<div class="buttons">
      <button type="button" disabled=${busy} onClick=${() => decide("approve")}>Approve</button>
      <button type="button" class="deny" disabled=${busy} onClick=${() => decide("reject")}>Reject</button>
      <span class="muted small">Approving runs every step above; nothing else asks again.</span>
    </div>`}
  </div>`;
}

function Nodes({ nodes, ctx }) {
  return nodes.map((n) => {
    switch (n.t) {
      case "block":
        if (n.b.kind === "plan") {
          return html`<${PlanCard} key=${n.key} p=${(ctx.plans && ctx.plans[n.b.plan_id]) || n.b.plan} sessionID=${ctx.sessionID} onPlan=${ctx.onPlan} />`;
        }
        return html`<${BlockView} key=${n.key} node=${n} />`;
      case "approval": return html`<${ApprovalCard} key=${n.key} a=${n.a} sessionID=${ctx.sessionID} onDecided=${ctx.onDecided} />`;
      case "chunk": return html`<${ChunkView} key=${n.key} c=${n} ctx=${ctx} />`;
      case "thinking": return html`<div key=${n.key} class="thinking-count muted">thinking ${fmtTokens(n.n)} tokens</div>`;
    }
    return null;
  });
}

/** A chunk keeps the viewer's own open/closed choice; otherwise it follows the turn. */
function ChunkView({ c, ctx }) {
  const user = ctx.overrides[c.key];
  const open = user === undefined ? c.open : user;
  const onToggle = (e) => {
    const now = e.currentTarget.open;
    if (now !== open) ctx.setOverride(c.key, now === c.open ? undefined : now);
  };
  return html`<details class=${cx("chunk", c.sub && "sub")} open=${open} onToggle=${onToggle}>
    <summary>${chunkSummary(c)}</summary>
    <div class="chunk-body"><${Nodes} nodes=${c.children} ctx=${ctx} /></div>
  </details>`;
}

/**
 * @param {{sessionID:string, blocks:any[], approvals:any[], drafts:Record<string,string>,
 *   running:boolean, showNoise:boolean, onDecided:(a:any)=>void, plans?:Record<string,any>, onPlan?:(p:any)=>void}} props
 */
export function Transcript({ sessionID, blocks, approvals, drafts, running, showNoise, onDecided, plans, onPlan }) {
  const tree = useMemo(() => buildTree(blocks, approvals), [blocks, approvals]);
  const [overrides, setOverrides] = useState({});
  useEffect(() => setOverrides({}), [sessionID]);
  const setOverride = useCallback((key, v) => {
    setOverrides((o) => {
      const n = { ...o };
      if (v === undefined) delete n[key];
      else n[key] = v;
      return n;
    });
  }, []);
  const ctx = { sessionID, onDecided, overrides, setOverride, plans, onPlan };
  const draftSeqs = Object.keys(drafts).map(Number).sort((a, b) => a - b);
  return html`<div class=${cx("log", showNoise && "show-noise")}>
    <${Nodes} nodes=${tree} ctx=${ctx} />
    ${draftSeqs.map((s) => html`<div key=${"draft-" + s} class="blk k-assistant draft"><div class="text plain">${drafts[s]}</div></div>`)}
    ${running && !draftSeqs.length && html`<div class="typing" aria-label="working"><span></span><span></span><span></span></div>`}
  </div>`;
}
