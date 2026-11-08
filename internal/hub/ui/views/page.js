// The frame of the non-session pages: the top bar and a scrolling body.
import { html } from "../lib.js";
import { TopBar } from "./session.js";
import { cx } from "../util.js";

export function Page({ app, title, wide, children, extra }) {
  return html`<div class="main">
    <${TopBar} app=${app} title=${title} extra=${extra} />
    <div class="scroller"><div class=${cx("page-body", wide && "wide")}>${children}</div></div>
  </div>`;
}

/** A key figure: label over a big value, optional note below. */
export function Stat({ label, value, note, tone }) {
  return html`<div class=${cx("stat", tone)}>
    <div class="stat-label">${label}</div>
    <div class="stat-value">${value}</div>
    ${note && html`<div class="stat-note muted">${note}</div>`}
  </div>`;
}

/** A user as a short label: name, else e-mail, else sub. */
export function personLabel(/** @type {{name?:string,email?:string,sub:string}|undefined} */ p, /** @type {string} */ sub) {
  if (!p) return sub;
  return p.name || p.email || p.sub;
}
