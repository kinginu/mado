// mado SPA entry (DESIGN §17.5): the layout, client-side routing by pushState, the
// session list for the sidebar. Plain ES modules, no build step.
import { html, render, useState, useEffect, useCallback, useRef } from "./lib.js";
import { get, setCSRF } from "./api.js";
import { Sidebar } from "./components/sidebar.js";
import { Toasts, toast } from "./components/toast.js";
import { SessionView } from "./views/session.js";
import { NewView } from "./views/new.js";
import { UsageView } from "./views/usage.js";
import { SettingsView } from "./views/settings.js";
import { AdminUsersView } from "./views/admin_users.js";
import { ProfilesView } from "./views/profiles.js";
import { HistoryListView, HistoryFileView } from "./views/history.js";
import { AuditView } from "./views/audit.js";
import { EnrollView } from "./views/enroll.js";
import { MachinesView } from "./views/machines.js";
import { InboxView } from "./views/inbox.js";
import { cx } from "./util.js";

/** Paths the SPA handles (ui.go serves the shell for the same list). */
const spaPaths = new Set(["/", "/new", "/usage", "/settings", "/admin/users", "/profiles", "/history", "/audit", "/admin/enroll", "/admin/machines", "/inbox"]);
function spaRoute(/** @type {string} */ path) {
  return spaPaths.has(path) || /^\/(s|history)\/[^/]+$/.test(path);
}

function readPanelPref() {
  try { return localStorage.getItem("mado.panel") !== "closed"; } catch (e) { return true; }
}

function App() {
  const [me, setMe] = useState(null);
  const [loc, setLoc] = useState({ path: location.pathname, search: location.search });
  const [sessions, setSessions] = useState([]);
  const [drawer, setDrawer] = useState(false);
  const [sheet, setSheet] = useState(false);
  const [panel, setPanel] = useState(readPanelPref);
  const refreshTimer = useRef(0);

  const refreshSessions = useCallback(() => {
    clearTimeout(refreshTimer.current);
    refreshTimer.current = setTimeout(() => {
      get("/api/sessions").then((r) => setSessions(r.sessions || []), () => {});
    }, 150);
  }, []);

  const navigate = useCallback((/** @type {string} */ to, replace = false) => {
    const u = new URL(to, location.origin);
    if (replace) history.replaceState(null, "", u.pathname + u.search);
    else history.pushState(null, "", u.pathname + u.search);
    setLoc({ path: u.pathname, search: u.search });
    setDrawer(false);
    setSheet(false);
  }, []);

  useEffect(() => {
    get("/api/me").then((m) => { setCSRF(m.csrf); setMe(m); }, (e) => toast(e.message));
    refreshSessions();
    const t = setInterval(refreshSessions, 15000);
    const onPop = () => setLoc({ path: location.pathname, search: location.search });
    window.addEventListener("popstate", onPop);
    // Same-origin links to SPA routes navigate in place.
    const onClick = (/** @type {MouseEvent} */ e) => {
      if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      const a = /** @type {HTMLElement} */ (e.target).closest("a");
      if (!a || a.target || a.hasAttribute("download")) return;
      const u = new URL(a.href, location.href);
      if (u.origin !== location.origin || !spaRoute(u.pathname)) return;
      e.preventDefault();
      navigate(u.pathname + u.search);
    };
    document.addEventListener("click", onClick);
    return () => { clearInterval(t); window.removeEventListener("popstate", onPop); document.removeEventListener("click", onClick); };
  }, []);

  useEffect(() => {
    const t = loc.path.startsWith("/s/") ? (sessions.find((s) => "/s/" + s.id === loc.path) || {}).title : null;
    document.title = t ? `${t} · mado` : "mado";
  }, [loc, sessions]);

  if (!me) return html`<div class="boot">mado</div>`;

  const togglePanel = () => {
    const v = !panel;
    setPanel(v);
    try { localStorage.setItem("mado.panel", v ? "open" : "closed"); } catch (e) { /* private mode */ }
  };
  // Opens the right panel (the sheet on phones) without changing the saved preference.
  const showPanel = () => { setPanel(true); setSheet(true); };
  const app = { me, navigate, refreshSessions, setDrawer, setSheet, togglePanel, showPanel };
  const m = /^\/s\/([^/]+)$/.exec(loc.path);
  const hm = /^\/history\/([^/]+)$/.exec(loc.path);
  const params = new URLSearchParams(loc.search);
  let view;
  if (m) view = html`<${SessionView} key=${m[1]} id=${decodeURIComponent(m[1])} app=${app} />`;
  else if (loc.path === "/admin/users") view = html`<${AdminUsersView} app=${app} />`;
  else if (loc.path === "/profiles") view = html`<${ProfilesView} app=${app} search=${loc.search} />`;
  else if (loc.path === "/history") view = html`<${HistoryListView} app=${app} search=${loc.search} />`;
  else if (hm) view = html`<${HistoryFileView} key=${hm[1]} app=${app} id=${decodeURIComponent(hm[1])} />`;
  else if (loc.path === "/audit") view = html`<${AuditView} app=${app} search=${loc.search} />`;
  else if (loc.path === "/admin/enroll") view = html`<${EnrollView} app=${app} />`;
  else if (loc.path === "/admin/machines") view = html`<${MachinesView} app=${app} />`;
  else if (loc.path === "/inbox" && me.inbox) view = html`<${InboxView} app=${app} search=${loc.search} />`;
  else if (loc.path === "/usage") view = html`<${UsageView} app=${app} search=${loc.search} />`;
  else if (loc.path === "/settings") view = html`<${SettingsView} app=${app} />`;
  else view = html`<${NewView} app=${app} kind=${params.get("kind") || ""}
    preset=${{ machine: params.get("machine") || "", cwd: params.get("cwd") || "" }} />`;

  return html`<div class=${cx("app", m ? "has-panel" : "no-panel", m && !panel && "panel-closed", drawer && "drawer-open", sheet && "sheet-open")}>
    <${Sidebar} me=${me} sessions=${sessions} current=${m ? m[1] : ""} path=${loc.path} />
    ${view}
    <div class="scrim" onClick=${() => { setDrawer(false); setSheet(false); }}></div>
    <${Toasts} />
  </div>`;
}

render(html`<${App} />`, /** @type {HTMLElement} */ (document.getElementById("app")));
