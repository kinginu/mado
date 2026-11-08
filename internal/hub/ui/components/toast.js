// Error and info toasts, bottom centre, dismissed after a few seconds.
import { html, useEffect, useState } from "../lib.js";

/** @type {Set<(t:{id:number,text:string,kind:string})=>void>} */
const listeners = new Set();
let next = 1;

/** @param {string} text @param {"error"|"info"} [kind] */
export function toast(text, kind = "error") {
  const t = { id: next++, text, kind };
  listeners.forEach((fn) => fn(t));
}

export function Toasts() {
  const [items, setItems] = useState([]);
  useEffect(() => {
    const fn = (t) => {
      setItems((xs) => [...xs.slice(-3), t]);
      setTimeout(() => setItems((xs) => xs.filter((x) => x.id !== t.id)), t.kind === "error" ? 7000 : 3000);
    };
    listeners.add(fn);
    return () => listeners.delete(fn);
  }, []);
  return html`<div class="toasts" role="status" aria-live="polite">
    ${items.map((t) => html`<div key=${t.id} class=${"toast " + t.kind}>
      <span>${t.text}</span>
      <button type="button" class="link" aria-label="dismiss" onClick=${() => setItems((xs) => xs.filter((x) => x.id !== t.id))}>×</button>
    </div>`)}
  </div>`;
}
