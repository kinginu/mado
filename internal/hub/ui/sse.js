// The session event stream (GET /s/{id}/events, JSON mode). EventSource reconnects by
// itself with Last-Event-ID after a dropped connection; when the server answers with
// an error (EventSource gives up, readyState CLOSED) we reconnect from the last seq we
// saw, with a growing delay.

/**
 * @param {string} sessionID
 * @param {() => number} lastSeq the last stored seq the page has
 * @param {Record<string, (data: any, ev: MessageEvent) => void>} handlers
 * @param {(connected: boolean) => void} onStatus
 * @returns {() => void} close
 */
export function openStream(sessionID, lastSeq, handlers, onStatus) {
  let es = null;
  let closed = false;
  let delay = 1000;
  let timer = 0;
  const connect = () => {
    if (closed) return;
    es = new EventSource(`/s/${encodeURIComponent(sessionID)}/events?format=json&after=${lastSeq()}`);
    es.onopen = () => { delay = 1000; onStatus(true); };
    es.onerror = () => {
      onStatus(false);
      if (es && es.readyState === EventSource.CLOSED) {
        es.close();
        timer = setTimeout(connect, delay);
        delay = Math.min(delay * 2, 30000);
      }
    };
    for (const [name, fn] of Object.entries(handlers)) {
      es.addEventListener(name, (/** @type {MessageEvent} */ ev) => {
        let data;
        try { data = JSON.parse(ev.data); } catch (e) { return; }
        fn(data, ev);
      });
    }
  };
  connect();
  return () => {
    closed = true;
    clearTimeout(timer);
    if (es) es.close();
  };
}
