// Small shared helpers.

/** Model id without its org prefix ("zai-org/glm-5.3" -> "glm-5.3"). */
export function shortModel(/** @type {string} */ m) {
  const i = (m || "").lastIndexOf("/");
  return i >= 0 ? m.slice(i + 1) : m || "";
}

export function fmtTokens(/** @type {number} */ n) {
  n = n || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1000) return (n / 1000).toFixed(1) + "k";
  return String(n);
}

/**
 * A cost or budget in the hub's [budgets] currency, the same as the hub's
 * formatMoney: "¥1,234" for JPY (no decimals), "$1.23" for USD, "EUR 1.23" otherwise.
 * A non-zero amount that would print as zero shows as "<¥1" / "<$0.01".
 */
export function fmtMoney(/** @type {number} */ x, /** @type {string} */ currency) {
  x = x || 0;
  const jpy = currency === "JPY";
  const digits = jpy ? 0 : 2;
  const prefix = jpy ? "¥" : currency === "USD" || !currency ? "$" : currency + " ";
  const s = (v) => prefix + v.toLocaleString("en-US", { minimumFractionDigits: digits, maximumFractionDigits: digits });
  if (x > 0 && x < (jpy ? 0.5 : 0.005)) return "<" + s(jpy ? 1 : 0.01);
  return s(x);
}

export function fmtBytes(/** @type {number} */ n) {
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  if (n >= 1024) return Math.round(n / 1024) + " KB";
  return n + " B";
}

/** The state word without the detail the hub may append ("exited: x", "starting (resuming)"). */
export function baseState(/** @type {string} */ s) {
  return (s || "").split(/[ :]/)[0];
}

/** Sessions show a live dot for these. */
export function isLive(/** @type {string} */ s) {
  const b = baseState(s);
  return b === "running" || b === "waiting_approval" || b === "starting";
}

/** Join class names, skipping falsy ones. */
export function cx(...names) {
  return names.filter(Boolean).join(" ");
}

/** An RFC 3339 time as a short local date and time ("2026-10-18 19:05"), "" for none. */
export function fmtTime(/** @type {string} */ iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  const p = (/** @type {number} */ n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/**
 * The permission mode as shown: auto on a router profile is mado's (DESIGN §18.1),
 * on a subscription profile Claude Code's own (§25.1).
 */
export function permissionLabel(s) {
  if (s.mado_auto) return "auto (mado decides)";
  if (s.permission_mode === "auto") return "auto (claude decides)";
  return s.permission_mode || "default";
}

/** The label of a mode in the panel's select (DESIGN §25.4); router tells auto apart. */
export function modeOptionLabel(mode, router) {
  if (mode === "auto") return router ? "auto (mado decides)" : "auto (claude decides)";
  return mode;
}

// Rate limits (DESIGN §26.3), in the browser's local time.

/** "02:30", or with the date "Nov 3 09:00", for unix seconds. */
export function fmtClock(/** @type {number} */ unix, /** @type {boolean} */ withDate) {
  const d = new Date(unix * 1000);
  const p = (/** @type {number} */ n) => String(n).padStart(2, "0");
  const hm = `${p(d.getHours())}:${p(d.getMinutes())}`;
  if (!withDate) return hm;
  return `${d.toLocaleDateString("en-US", { month: "short", day: "numeric" })} ${hm}`;
}

/** The browser's short time zone name ("JST"), "" when unknown. */
export function tzName() {
  try {
    const part = new Intl.DateTimeFormat("en-US", { timeZoneName: "short" }).formatToParts(new Date())
      .find((x) => x.type === "timeZoneName");
    return part ? part.value : "";
  } catch (e) { return ""; }
}

/** "2 h 14 m" from now until unix seconds ("" when past). */
export function fmtUntil(/** @type {number} */ unix) {
  const m = Math.ceil((unix * 1000 - Date.now()) / 60000);
  if (m <= 0) return "";
  const d = Math.floor(m / 1440), h = Math.floor((m % 1440) / 60), r = m % 60;
  return [d && `${d} d`, h && `${h} h`, (r || (!d && !h)) && `${r} m`].filter(Boolean).join(" ");
}

/** The window name of a rateLimitType ("five_hour" -> "5-hour"). */
export function windowName(/** @type {string} */ t) {
  return t === "five_hour" ? "5-hour" : t === "seven_day" ? "7-day" : (t || "usage").replace(/_/g, " ");
}
