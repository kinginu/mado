// Tool chunks (DESIGN §15), rebuilt from the JSON blocks on every change. A run of
// tool calls, results, approvals and status lines between two texts becomes one
// chunk ("Bash ×3, Read ×1"), open while the turn runs and collapsed at its result.
// Subagent blocks (parent = the Agent call's tool id) nest in a chunk placed right
// after that call, labelled "Explore · deepseek-v4-flash". A tool result joins its
// call's chunk even when it arrives later. thinking_tokens deltas become one counter
// per turn, shown on the turn's result line (or live at the end while it runs).
// An image a tool generated (file block, origin "generated") stays outside the chunks,
// and so do the concierge's plan cards and mado's notes (DESIGN §20.3).
// The session view and the history view (/history/{id}) both use it.

import { shortModel } from "../util.js";

/**
 * @typedef {{seq:number, kind:string, label:string, html?:string, text:string, detail?:string,
 *   is_error:boolean, subagent:boolean, parent?:string, tool_id?:string, agent?:string, model?:string,
 *   noise:boolean, delta?:number, file_id?:string, mime?:string, origin?:string}} Block
 * @typedef {{request_id:string, tool_name:string, summary:string, detail?:string, state:string,
 *   decision?:string, decided_by?:string, auto?:string, reason?:string, after_seq?:number, afterSeq:number}} Approval
 * @typedef {{t:"block", key:string, b:Block, thinking?:number, tool?:string}} BlockNode
 * @typedef {{t:"approval", key:string, a:Approval}} ApprovalNode
 * @typedef {{t:"chunk", key:string, label:string, counts:Record<string,number>, order:string[],
 *   children:Node[], open:boolean, sub:boolean, agent?:string, model?:string, denied?:number}} ChunkNode
 * @typedef {{t:"thinking", key:string, n:number}} ThinkingNode
 * @typedef {BlockNode|ApprovalNode|ChunkNode|ThinkingNode} Node
 */

/** @param {ChunkNode} c */
export function chunkSummary(c) {
  const parts = c.order.map((n) => `${n} ×${c.counts[n]}`);
  let text = parts.length ? parts.join(", ") : "activity";
  if (c.denied) text += ` · ${c.denied} auto-denied`; // visible while the chunk is closed (DESIGN §18.3)
  return c.label ? `${c.label} · ${text}` : text;
}

/**
 * @param {Block[]} blocks in seq order
 * @param {Approval[]} approvals each placed after the blocks with seq <= afterSeq
 * @returns {Node[]}
 */
export function buildTree(blocks, approvals) {
  /** @type {Node[]} */
  const top = [];
  /** @type {ChunkNode|null} */
  let cur = null;
  let thinking = 0;
  /** @type {ChunkNode[]} */
  const chunks = [];
  /** tool id -> where its tool_use node sits */
  const tools = new Map();
  /** Agent tool id -> its subagent chunk */
  const subs = new Map();

  const makeChunk = (/** @type {string} */ key, sub) => {
    /** @type {ChunkNode} */
    const c = { t: "chunk", key, label: "", counts: {}, order: [], children: [], open: true, sub };
    chunks.push(c);
    return c;
  };
  const add = (/** @type {ChunkNode} */ c, /** @type {Node} */ n) => {
    c.children.push(n);
    if (n.t === "block" && n.b.kind === "tool_use") {
      const tool = n.b.label;
      if (!c.counts[tool]) { c.counts[tool] = 0; c.order.push(tool); }
      c.counts[tool]++;
    }
  };
  const currentChunk = (/** @type {string} */ key) => {
    if (!cur) { cur = makeChunk("c" + key, false); top.push(cur); }
    return cur;
  };
  const subChunk = (/** @type {string} */ agentID) => {
    let sc = subs.get(agentID);
    if (!sc) {
      const at = tools.get(agentID);
      sc = makeChunk("sub-" + agentID, true);
      sc.agent = at.node.b.agent || "subagent";
      sc.label = sc.agent + (at.node.b.model ? " · " + at.node.b.model : "");
      const i = at.list.indexOf(at.node);
      at.list.splice(i + 1, 0, sc);
      subs.set(agentID, sc);
    }
    return sc;
  };
  const register = (/** @type {BlockNode} */ n, /** @type {Node[]} */ list) => {
    if (n.b.kind === "tool_use" && n.b.tool_id) tools.set(n.b.tool_id, { node: n, list });
  };

  const placeApproval = (/** @type {Approval} */ a) => {
    const c = currentChunk("ap-" + a.request_id);
    if (a.state === "pending") c.open = true;
    if (a.auto === "deny" && a.state !== "pending") c.denied = (c.denied || 0) + 1;
    add(c, { t: "approval", key: "ap-" + a.request_id, a });
  };

  const aps = [...approvals].sort((x, y) => x.afterSeq - y.afterSeq);
  let ai = 0;

  // Keys are seq + the block's index within its event, so they survive prepending an
  // earlier page (DESIGN §17.14) and a chunk keeps its viewer's open/closed choice.
  let prevSeq = -1;
  let inSeq = 0;
  blocks.forEach((b) => {
    while (ai < aps.length && aps[ai].afterSeq < b.seq) placeApproval(aps[ai++]);
    inSeq = b.seq === prevSeq ? inSeq + 1 : 0;
    prevSeq = b.seq;
    const key = b.seq + "-" + inSeq;
    if (b.kind === "thinking_tokens") {
      thinking += b.delta || 0;
      return;
    }
    /** @type {BlockNode} */
    const n = { t: "block", key, b };
    if (b.parent && tools.has(b.parent)) {
      const sc = subChunk(b.parent);
      if (b.model && !sc.model && (b.kind === "assistant" || (b.kind === "tool_use" && !b.agent))) {
        sc.model = b.model;
        sc.label = sc.agent + " · " + shortModel(b.model);
      }
      add(sc, n);
      register(n, sc.children);
      return;
    }
    if (b.kind === "tool_result") {
      const tu = tools.get(b.tool_id || "");
      if (tu) n.tool = tu.node.b.label; // the result view knows its tool (search results)
      if (tu && tu.list !== top) { tu.list.push(n); return; }
    }
    switch (b.kind) {
      case "tool_use":
      case "tool_result":
      case "status": {
        const c = currentChunk(key);
        add(c, n);
        register(n, c.children);
        return;
      }
      case "result":
        for (const c of chunks) c.open = false;
        if (thinking > 0) n.thinking = thinking;
        thinking = 0;
        cur = null;
        top.push(n);
        return;
      case "file":
        // A tool's image is shown between the chunks, never folded into one.
        if (b.origin === "generated") { cur = null; top.push(n); return; }
        if (cur) add(cur, n); else top.push(n);
        return;
      case "user":
      case "assistant":
      case "note":
      case "plan":
        cur = null;
        top.push(n);
        return;
      default:
        if (cur) add(cur, n); // thinking, system lines, raw: inside the open chunk
        else top.push(n);
    }
  });
  while (ai < aps.length) placeApproval(aps[ai++]);
  if (thinking > 0) top.push({ t: "thinking", key: "thinking", n: thinking });
  return top;
}
