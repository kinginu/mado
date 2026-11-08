// Inline SVG icons (stroke = currentColor). No external files, no style attributes.
import { html } from "../lib.js";

const svg = (path, cls = "icon") => html`<svg class=${cls} viewBox="0 0 24 24" width="18" height="18" fill="none"
  stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${path}</svg>`;

export const IconChat = () => svg(html`<path d="M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.6A8 8 0 1 1 21 12z" />`);
export const IconCode = () => svg(html`<path d="M4 17l6-5-6-5" /><path d="M12 19h8" />`);
export const IconPlus = () => svg(html`<path d="M12 5v14M5 12h14" />`);
export const IconMenu = () => svg(html`<path d="M4 6h16M4 12h16M4 18h16" />`);
export const IconInfo = () => svg(html`<circle cx="12" cy="12" r="9" /><path d="M12 11v5M12 8h.01" />`);
export const IconClip = () => svg(html`<path d="M21 11.5l-8.6 8.6a5 5 0 0 1-7.1-7.1l8.6-8.6a3.3 3.3 0 0 1 4.7 4.7l-8.6 8.6a1.7 1.7 0 0 1-2.4-2.4l7.9-7.9" />`);
export const IconSend = () => svg(html`<path d="M12 19V5M5 12l7-7 7 7" />`);
export const IconStop = () => svg(html`<rect x="6" y="6" width="12" height="12" rx="2" />`);
export const IconSearch = () => svg(html`<circle cx="11" cy="11" r="7" /><path d="M20 20l-3.5-3.5" />`);
export const IconPanel = () => svg(html`<rect x="3" y="4" width="18" height="16" rx="2" /><path d="M15 4v16" />`);
/** The concierge (DESIGN §20): a compass. */
export const IconPlan = () => svg(html`<circle cx="12" cy="12" r="9" /><path d="M15.5 8.5l-2 5-5 2 2-5z" />`);
export const IconClose = () => svg(html`<path d="M6 6l12 12M18 6L6 18" />`);
/** The inbox (DESIGN §23): a tray. */
export const IconInbox = () => svg(html`<path d="M3 13l3-8h12l3 8v6H3z" /><path d="M3 13h5l1 2h6l1-2h5" />`);
/** A digest session: lines of a summary. */
export const IconDigest = () => svg(html`<path d="M5 6h14M5 10h14M5 14h9M5 18h6" />`);
