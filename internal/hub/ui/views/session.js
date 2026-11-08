// /s/{id}: transcript, composer and the right panel. The transcript loads tail first
// (DESIGN §17.14): one GET /api/sessions/{id}/blocks?before=0 for the newest page, then
// the SSE stream continues after its last seq; "Load earlier" (or scrolling to the top)
// prepends the page before the first loaded seq. Deltas show the raw text of a
// streaming chat answer until its stored block arrives.
import { html, useState, useEffect, useRef, useLayoutEffect, useCallback, useMemo } from "../lib.js";
import { api, get } from "../api.js";
import { openStream } from "../sse.js";
import { Transcript, PathContext } from "../components/transcript.js";
import { Composer } from "../components/composer.js";
import { Panel } from "../components/panel.js";
import { toast } from "../components/toast.js";
import { IconInfo, IconMenu, IconPanel } from "../components/icons.js";
import { baseState, cx, fmtClock, fmtUntil, tzName, windowName } from "../util.js";

const runningStates = new Set(["running", "waiting_approval"]);

export function SessionView({ id, app }) {
  const [s, setS] = useState(null);
  const [detail, setDetail] = useState(null);
  const [models, setModels] = useState(null);
  const [blocks, setBlocks] = useState([]);
  const [approvals, setApprovals] = useState([]);
  const [drafts, setDrafts] = useState({});
  const [loaded, setLoaded] = useState(false);
  const [missing, setMissing] = useState(false);
  const [connected, setConnected] = useState(true);
  const [showNoise, setShowNoise] = useState(false);
  const [chatModels, setChatModels] = useState([]);
  const [renaming, setRenaming] = useState(false);
  const [drop, setDrop] = useState(null);
  const [dragging, setDragging] = useState(false);
  const [first, setFirst] = useState(0); // the oldest loaded seq
  const [moreBefore, setMoreBefore] = useState(false);
  const [earlier, setEarlier] = useState(false); // a "Load earlier" request is running
  const [plans, setPlans] = useState({}); // concierge plan id -> its latest state (SSE "plan")
  const [restart, setRestart] = useState(false); // a mode change waits for a restart (DESIGN §25.3)
  const last = useRef(0);
  const scroller = useRef(null);
  const atBottom = useRef(true);
  /** scrollHeight and scrollTop before a prepend, restored by the layout effect */
  const anchor = useRef(null);
  const busy = useRef(false);
  const gen = useRef(0); // bumps on every session switch; stale "Load earlier" answers are dropped
  const fileView = useFileView(app);

  const loadDetail = useCallback(async () => {
    try {
      const d = await get(`/api/sessions/${id}`);
      setS(d.session);
      setDetail(d);
      if (d.models) setModels(d.models);
      return d;
    } catch (e) {
      if (e.status === 404) setMissing(true);
      else toast(e.message);
      return null;
    }
  }, [id]);

  // Auto decisions can come in bursts; refetch the detail at most every half second.
  const autoTimer = useRef(0);
  const refreshAuto = useCallback(() => {
    if (autoTimer.current) return;
    autoTimer.current = setTimeout(() => { autoTimer.current = 0; loadDetail(); }, 500);
  }, [loadDetail]);
  useEffect(() => () => clearTimeout(autoTimer.current), []);

  // Load the newest page, then stream.
  useEffect(() => {
    let close = () => {};
    let cancelled = false;
    setS(null); setDetail(null); setModels(null); setBlocks([]); setApprovals([]); setDrafts({});
    setLoaded(false); setMissing(false); setRenaming(false);
    setFirst(0); setMoreBefore(false); setEarlier(false); setPlans({}); setRestart(false);
    last.current = 0;
    atBottom.current = true;
    anchor.current = null;
    busy.current = false;
    gen.current++;
    (async () => {
      const [d, r] = await Promise.all([
        loadDetail(),
        get(`/api/sessions/${id}/blocks?before=0`).catch((e) => { if (e.status !== 404) toast(e.message); return null; }),
      ]);
      if (!d || cancelled) return;
      if (d.session.kind === "chat") get("/api/chat/models").then((r) => setChatModels(r.models || []), () => {});
      if (r) {
        last.current = r.last;
        setBlocks(r.blocks);
        setFirst(r.first);
        setMoreBefore(!!r.more_before);
        setApprovals((r.approvals || []).map((a) => ({ ...a, afterSeq: a.after_seq || last.current })));
      }
      setLoaded(true);
      close = openStream(id, () => last.current, {
        block: (bs) => {
          if (!Array.isArray(bs) || bs.length === 0 || bs[0].seq <= last.current) return;
          const seq = bs[0].seq;
          last.current = seq;
          setBlocks((xs) => xs.concat(bs));
          setDrafts((ds) => {
            const ks = Object.keys(ds).filter((k) => Number(k) > seq);
            if (ks.length === Object.keys(ds).length) return ds;
            const n = {};
            ks.forEach((k) => { n[k] = ds[k]; });
            return n;
          });
        },
        approval: (a) => {
          if (a.auto) refreshAuto(); // the panel's "Auto decisions" counts
          setApprovals((xs) => {
            const i = xs.findIndex((x) => x.request_id === a.request_id);
            if (i < 0) return xs.concat({ ...a, afterSeq: a.after_seq || last.current });
            const n = xs.slice();
            n[i] = { ...a, afterSeq: xs[i].afterSeq };
            return n;
          });
        },
        delta: (d) => {
          if (d.seq <= last.current) return;
          setDrafts((ds) => ({ ...ds, [d.seq]: (ds[d.seq] || "") + d.text }));
        },
        state: (d) => {
          setS((x) => x && { ...x, state: d.state, running: runningStates.has(baseState(d.state)) });
          app.refreshSessions();
          if (!runningStates.has(baseState(d.state))) loadDetail();
        },
        models: (m) => setModels(m),
        // The session's view changed on the hub (mode, rate limit; DESIGN §25.3, §26.1).
        session: (v) => { setS((x) => x && { ...x, ...v }); loadDetail(); },
        plan: (p) => {
          setPlans((ps) => ({ ...ps, [p.id]: p }));
          if (p.state === "done" || p.state === "failed") { loadDetail(); app.refreshSessions(); }
        },
      }, setConnected);
    })();
    return () => { cancelled = true; close(); };
  }, [id]);

  // The page before the oldest loaded seq, prepended with the viewport kept in place.
  const loadEarlier = useCallback(async () => {
    if (busy.current || !moreBefore) return;
    busy.current = true;
    setEarlier(true);
    const g = gen.current;
    try {
      const r = await get(`/api/sessions/${id}/blocks?before=${first}`);
      if (g !== gen.current) return;
      const el = scroller.current;
      anchor.current = el ? { height: el.scrollHeight, top: el.scrollTop } : null;
      setBlocks((xs) => r.blocks.concat(xs));
      if (r.first) setFirst(r.first);
      setMoreBefore(!!r.more_before);
    } catch (e) {
      toast(e.message);
    } finally {
      if (g === gen.current) { busy.current = false; setEarlier(false); }
    }
  }, [id, first, moreBefore]);

  // Stay at the bottom when the viewer was there; keep the position otherwise. Reaching
  // the top loads the earlier page.
  const onScroll = () => {
    const el = scroller.current;
    if (!el) return;
    atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    if (el.scrollTop < 200 && moreBefore && loaded) loadEarlier();
  };
  useLayoutEffect(() => {
    const el = scroller.current;
    if (!el) return;
    const a = anchor.current;
    if (a) {
      anchor.current = null;
      el.scrollTop = a.top + (el.scrollHeight - a.height);
      return;
    }
    if (atBottom.current) el.scrollTop = el.scrollHeight;
  }, [blocks, drafts, approvals, loaded]);

  // Decided approvals from before the oldest loaded block wait for their page.
  const shown = useMemo(() => (moreBefore
    ? approvals.filter((a) => a.state === "pending" || a.afterSeq >= first)
    : approvals), [approvals, moreBefore, first]);

  const onPlan = useCallback((p) => setPlans((ps) => ({ ...ps, [p.id]: p })), []);

  const onDecided = useCallback((a) => {
    setApprovals((xs) => xs.map((x) => (x.request_id === a.request_id ? { ...a, afterSeq: x.afterSeq } : x)));
  }, []);

  if (missing) {
    return html`<div class="main"><${TopBar} app=${app} title="Not found" /><div class="scroller"><div class="empty-state">
      <h2>No such session</h2><p class="muted">It was deleted, or it belongs to someone else.</p>
      <a class="button" href="/">Back</a></div></div></div>`;
  }

  const running = !!(s && s.running);
  const interrupt = async () => {
    try { await api("POST", `/api/sessions/${id}/interrupt`); toast("interrupt sent", "info"); } catch (e) { toast(e.message); }
  };
  const resume = async () => {
    try {
      const r = await api("POST", `/api/sessions/${id}/resume`);
      app.refreshSessions();
      toast("resumed", "info");
      app.navigate("/s/" + r.id);
    } catch (e) {
      toast(e.message);
      if (e.data && e.data.id) app.navigate("/s/" + e.data.id);
    }
  };
  const actions = {
    rename: () => setRenaming(true),
    interrupt,
    stop: async () => {
      try { await api("POST", `/api/sessions/${id}/stop`); toast("stop sent", "info"); } catch (e) { toast(e.message); }
    },
    resume,
    // DESIGN §25.3: tell the live claude; when it cannot be told, offer a restart.
    setMode: async (mode) => {
      try {
        const r = await api("POST", `/api/sessions/${id}/mode`, { mode });
        setS((x) => x && { ...x, ...r.session });
        setDetail((d) => d && { ...d, auto: r.auto });
        setRestart(!!r.restart);
        app.refreshSessions();
        toast(r.restart ? `permission mode: ${mode} (restart to apply)` : `permission mode: ${mode}`, "info");
      } catch (e) { toast(e.message); }
    },
    // "Restart to apply": Stop, wait for the session to end, then Resume (§21.6).
    restart: async () => {
      try {
        await api("POST", `/api/sessions/${id}/stop`);
        toast("restarting…", "info");
        for (let i = 0; i < 60; i++) {
          await new Promise((res) => setTimeout(res, 1000));
          const d = await get(`/api/sessions/${id}`);
          if (d.session.resumable) { setRestart(false); await resume(); return; }
        }
        toast("the session did not stop; try Stop, then Resume");
      } catch (e) { toast(e.message); }
    },
    setAuto: async (on) => {
      try {
        const was = s && s.mado_auto;
        const r = await api("PATCH", `/api/sessions/${id}`, { auto: on });
        setDetail((d) => d && { ...d, auto: r.auto });
        setS((x) => x && { ...x, permission_mode: r.session.permission_mode, mado_auto: r.session.mado_auto });
        app.refreshSessions();
        toast(on ? (was ? "mado decides approvals again" : "mado decides approvals from now on") : "approvals go to you now", "info");
      } catch (e) { toast(e.message); }
    },
    // DESIGN §26.2: the per-session auto-resume switch.
    setAutoResume: async (on) => {
      try {
        const r = await api("POST", `/api/sessions/${id}/auto_resume`, { on });
        setS((x) => x && { ...x, ...r.session });
        toast(on ? "resumes automatically when the limit resets" : "automatic resume is off", "info");
      } catch (e) { toast(e.message); }
    },
    remove: async () => {
      if (!confirm(`Delete "${(s && s.title) || "this session"}" with its messages and files?`)) return;
      try {
        await api("DELETE", `/api/sessions/${id}`);
        app.refreshSessions();
        app.navigate("/");
      } catch (e) { toast(e.message); }
    },
  };
  const saveTitle = async (title) => {
    setRenaming(false);
    if (!s || title.trim() === s.title) return;
    try {
      const r = await api("PATCH", `/api/sessions/${id}`, { title });
      setS((x) => ({ ...x, title: r.session.title }));
      app.refreshSessions();
    } catch (e) { toast(e.message); }
  };
  const onModel = async (model) => {
    try {
      const r = await api("PATCH", `/api/sessions/${id}`, { model });
      setS((x) => ({ ...x, model: r.session.model }));
    } catch (e) { toast(e.message); }
  };
  const onSent = (r) => {
    atBottom.current = true;
    if (r.id && r.id !== id) app.navigate("/s/" + r.id); // `/new …` started a session
    app.refreshSessions();
    if (s && s.kind === "chat") setS((x) => ({ ...x, running: true }));
  };
  // A continued session (DESIGN §21.6): follow resumed_by to the newest session.
  const openLatest = async () => {
    let cur = s && s.resumed_by;
    try {
      for (let i = 0; cur && i < 50; i++) {
        const d = await get(`/api/sessions/${cur}`);
        if (!d.session.resumed_by) break;
        cur = d.session.resumed_by;
      }
    } catch (e) { /* open the furthest one reached */ }
    if (cur) app.navigate("/s/" + cur);
  };
  const pending = approvals.filter((a) => a.state === "pending");
  const noiseToggle = html`<label class="noise-toggle" title="Show hidden diagnostics such as [claude-code:unrecognized_model] and auto-allowed approvals">
    <input type="checkbox" checked=${showNoise} onChange=${(e) => setShowNoise(e.currentTarget.checked)} /> diagnostics</label>`;
  const isChat = s && s.kind === "chat";

  const dragProps = isChat ? {
    onDragOver: (e) => { if (e.dataTransfer && [...e.dataTransfer.types].includes("Files")) { e.preventDefault(); setDragging(true); } },
    onDragLeave: (e) => { if (e.currentTarget === e.target) setDragging(false); },
    onDrop: (e) => {
      e.preventDefault();
      setDragging(false);
      if (e.dataTransfer && e.dataTransfer.files.length) setDrop([...e.dataTransfer.files]);
    },
  } : {};

  return html`
    <div class=${cx("main", dragging && "dragging")} ...${dragProps}>
      <${TopBar} app=${app} title=${s ? s.title || "Untitled" : ""} renaming=${renaming} onRename=${saveTitle}
        onStartRename=${() => setRenaming(true)} state=${s && s.state} connected=${connected}
        extra=${noiseToggle} />
      <div class="scroller" ref=${scroller} onScroll=${onScroll}>
        ${s && s.resumed_by && html`<div class="notice continued">
          This session continues in <a href=${"/s/" + s.resumed_by}>a newer session</a>.
          <button type="button" class="secondary" onClick=${openLatest}>Open latest</button></div>`}
        ${s && s.limited_until > 0 && html`<${LimitBanner} s=${s} setAutoResume=${actions.setAutoResume} />`}
        ${!loaded && html`<div class="muted loading">Loading…</div>`}
        ${loaded && moreBefore && html`<div class="load-earlier">
          <button type="button" class="secondary" disabled=${earlier} onClick=${loadEarlier}>${earlier ? "Loading…" : "Load earlier"}</button></div>`}
        ${loaded && blocks.length === 0 && !Object.keys(drafts).length && html`<div class="empty-state small muted">
          ${s && s.concierge ? "Tell mado what you want to start: the machine, the project, the goal. It looks around, then proposes a plan for you to approve."
            : isChat ? "Say something to start." : "The session is ready. Messages go to claude on " + (s ? s.machine : "the machine") + "."}</div>`}
        <${PathContext.Provider} value=${s && s.kind !== "chat" && s.machine_id ? fileView.open : null}>
        <${Transcript} sessionID=${id} blocks=${blocks} approvals=${shown} drafts=${drafts}
          running=${running} showNoise=${showNoise} onDecided=${onDecided} plans=${plans} onPlan=${onPlan} />
        <//>
      </div>
      <div class="composer-wrap">
        ${s && s.kind === "digest" ? html`<div class="notice digest-note">An inbox digest run; it takes no messages.
            <a href="/inbox?tab=digests">Inbox → Digests</a>
            ${running && html`<button type="button" class="secondary" onClick=${interrupt}>Stop</button>`}</div>`
          : s && html`<${Composer} s=${s} chatModels=${chatModels} running=${running} onSent=${onSent}
          onInterrupt=${interrupt} onModel=${onModel} dropFiles=${drop} clearDrop=${() => setDrop(null)} />`}
      </div>
    </div>
    <${Panel} s=${s} detail=${detail} models=${models} pending=${pending} actions=${actions} restart=${restart}
      onDecided=${onDecided} onClose=${() => app.setSheet(false)} noise=${noiseToggle} currency=${app.me.currency}
      file=${fileView.file} />`;
}

/**
 * The file viewer's state (DESIGN §27.3): a back stack of up to 20 paths. Opening a
 * path shows the panel (the sheet on phones); Back pops, close clears the stack.
 */
function useFileView(app) {
  const [stack, setStack] = useState(/** @type {string[]} */ ([]));
  const open = useCallback((/** @type {string} */ p) => {
    if (!p) return;
    setStack((xs) => (xs[xs.length - 1] === p ? xs : xs.concat(p).slice(-20)));
    app.showPanel();
  }, []);
  const file = stack.length ? {
    path: stack[stack.length - 1],
    depth: stack.length,
    open,
    back: () => setStack((xs) => xs.slice(0, -1)),
    close: () => { setStack([]); app.setSheet(false); },
  } : null;
  return { open, file };
}

/**
 * The usage-limit banner (DESIGN §26.3): the window, when the session resumes by
 * itself (local time; the 7-day window with the date), and the switch. The composer
 * stays usable; what is typed meanwhile is sent again at the reset.
 */
export function LimitBanner({ s, setAutoResume }) {
  const [, tick] = useState(0);
  useEffect(() => {
    const t = setInterval(() => tick((n) => n + 1), 30000);
    return () => clearInterval(t);
  }, []);
  const at = s.limited_until + 90; // the hub resumes 90 s after the reset
  const week = s.limited_type === "seven_day";
  const tz = tzName();
  const when = `${week ? "on " : "at "}${fmtClock(at, week)}${tz ? ` (${tz})` : ""}`;
  const left = fmtUntil(at);
  return html`<div class="notice limited" role="status">
    <span>Usage limit reached (${windowName(s.limited_type)} window).
      ${s.auto_resume ? ` Resumes automatically ${when}${left ? ` · in ${left}` : ""}.` : ` Resets ${when}${left ? ` · in ${left}` : ""}; automatic resume is off.`}</span>
    <label class="switch small"><input type="checkbox" checked=${!!s.auto_resume}
      onChange=${(e) => setAutoResume(e.currentTarget.checked)} /> <span>Resume automatically when the limit resets</span></label>
  </div>`;
}

/** The centre column's top bar: drawer button (phones), title, state, panel toggle. */
export function TopBar({ app, title, renaming, onRename, onStartRename, state, connected = true, extra }) {
  const input = useRef(null);
  useEffect(() => { if (renaming && input.current) { input.current.focus(); input.current.select(); } }, [renaming]);
  return html`<header class="topbar">
    <button type="button" class="icon-btn menu-btn" aria-label="Open sidebar" onClick=${() => app.setDrawer(true)}><${IconMenu} /></button>
    ${renaming
      ? html`<input ref=${input} class="title-input" value=${title} maxlength="120" aria-label="Title"
          onKeyDown=${(e) => { if (e.key === "Enter") onRename(e.currentTarget.value); if (e.key === "Escape") onRename(title); }}
          onBlur=${(e) => onRename(e.currentTarget.value)} />`
      : html`<h1 class="title" title=${onStartRename ? "Click to rename" : title} onClick=${onStartRename}>${title}</h1>`}
    ${state && html`<span class=${"state s-" + baseState(state)}>${state}</span>`}
    ${!connected && html`<span class="state s-offline" title="Reconnecting to the event stream">reconnecting</span>`}
    <span class="spacer"></span>
    ${extra}
    ${onStartRename && html`<button type="button" class="icon-btn info-btn" aria-label="Session details" onClick=${() => app.setSheet(true)}><${IconInfo} /></button>
      <button type="button" class="icon-btn panel-btn" aria-label="Toggle details panel" onClick=${() => app.togglePanel()}><${IconPanel} /></button>`}
  </header>`;
}
