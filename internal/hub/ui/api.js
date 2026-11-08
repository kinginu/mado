// JSON API client (DESIGN §17.5): cookie auth, X-CSRF-Token on every non-GET call.
// The browser adds the Origin header to same-origin POST/PATCH/DELETE requests.

let csrf = "";

/** @param {string} token */
export function setCSRF(token) { csrf = token; }
export function getCSRF() { return csrf; }

export class APIError extends Error {
  /** @param {number} status @param {string} message @param {any} [data] */
  constructor(status, message, data) {
    super(message);
    this.status = status;
    /** @type {any} */
    this.data = data || null;
  }
}

const fallback = {
  400: "the hub refused the request",
  403: "not allowed",
  404: "not found",
  402: "the monthly budget is used up",
  409: "busy: a turn is still running",
  413: "too large",
  502: "the upstream failed",
  503: "the machine is unreachable",
};

/**
 * @param {string} method
 * @param {string} path
 * @param {any} [body] JSON value, or FormData for uploads
 * @returns {Promise<any>}
 */
export async function api(method, path, body) {
  /** @type {RequestInit} */
  const opt = { method, headers: {}, credentials: "same-origin" };
  const headers = /** @type {Record<string,string>} */ (opt.headers);
  if (body instanceof FormData) {
    opt.body = body;
  } else if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  if (method !== "GET") headers["X-CSRF-Token"] = csrf;
  let r;
  try {
    r = await fetch(path, opt);
  } catch (e) {
    throw new APIError(0, "the hub is unreachable");
  }
  if (r.status === 401) {
    location.href = "/login";
    throw new APIError(401, "not logged in");
  }
  const text = await r.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch (e) { data = null; }
  }
  if (!r.ok) {
    const msg = (data && data.error) || fallback[r.status] || `request failed (${r.status})`;
    throw new APIError(r.status, msg, data);
  }
  return data;
}

export const get = (/** @type {string} */ p) => api("GET", p);
