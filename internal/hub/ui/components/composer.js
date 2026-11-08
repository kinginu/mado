// The composer (DESIGN §17.5): Enter sends, Shift+Enter is a newline, Esc
// interrupts. Chat sessions take files (attach button, paste, drop: POST /api/files,
// then file_ids in the send) and have a model picker; code sessions show their
// permission mode and accept `/new <machine> <project> [profile] [mode]`.
import { html, useState, useRef, useEffect } from "../lib.js";
import { api } from "../api.js";
import { toast } from "./toast.js";
import { IconClip, IconSend, IconStop } from "./icons.js";
import { cx, fmtBytes, permissionLabel } from "../util.js";

let nextLocal = 1;

/**
 * @param {{s:any, chatModels:any[], running:boolean, onSent:(r:any)=>void, onInterrupt:()=>void,
 *   onModel:(m:string)=>void, dropFiles:File[]|null, clearDrop:()=>void}} p
 */
export function Composer({ s, chatModels, running, onSent, onInterrupt, onModel, dropFiles, clearDrop }) {
  const [text, setText] = useState("");
  const [files, setFiles] = useState([]); // {local, name, size, id?, error?}
  const [sending, setSending] = useState(false);
  const ta = useRef(null);
  const fileInput = useRef(null);
  const isChat = s.kind === "chat";

  useEffect(() => { setText(""); setFiles([]); if (ta.current) ta.current.focus(); }, [s.id]);
  useEffect(() => {
    if (dropFiles && dropFiles.length) { addFiles(dropFiles); clearDrop(); }
  }, [dropFiles]);

  const addFiles = (/** @type {File[]} */ list) => {
    if (!isChat) { toast("code sessions take no attachments"); return; }
    for (const f of list) {
      const local = nextLocal++;
      setFiles((xs) => [...xs, { local, name: f.name || "pasted", size: f.size }]);
      const fd = new FormData();
      fd.append("session_id", s.id);
      fd.append("file", f, f.name || "pasted");
      api("POST", "/api/files", fd).then(
        (r) => setFiles((xs) => xs.map((x) => (x.local === local ? { ...x, id: r.id, mime: r.mime } : x))),
        (e) => {
          toast(`${f.name || "file"}: ${e.message}`);
          setFiles((xs) => xs.filter((x) => x.local !== local));
        });
    }
  };
  const removeFile = (x) => {
    setFiles((xs) => xs.filter((y) => y.local !== x.local));
    if (x.id) api("DELETE", "/api/files/" + encodeURIComponent(x.id)).catch(() => {});
  };

  const uploading = files.some((f) => !f.id);
  const canSend = !sending && !uploading && text.trim() !== "";
  // Keys can arrive faster than Preact re-renders (paste then Enter, IME commits), so
  // send reads the textarea itself, and the latest files through a ref.
  const filesRef = useRef(files);
  filesRef.current = files;
  const sendingRef = useRef(false);

  const send = async () => {
    const value = ta.current ? ta.current.value : text;
    const fs = filesRef.current;
    if (sendingRef.current || fs.some((f) => !f.id) || value.trim() === "") return;
    sendingRef.current = true;
    setSending(true);
    try {
      const body = { text: value };
      if (isChat) body.file_ids = fs.map((f) => f.id);
      const r = await api("POST", `/api/sessions/${s.id}/send`, body);
      setText("");
      setFiles([]);
      onSent(r || {});
    } catch (e) {
      toast(e.message);
    } finally {
      sendingRef.current = false;
      setSending(false);
      if (ta.current) ta.current.focus();
    }
  };

  const onKeyDown = (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && e.keyCode !== 229) {
      e.preventDefault();
      send();
    } else if (e.key === "Escape" && running) {
      e.preventDefault();
      onInterrupt();
    }
  };
  const onPaste = (e) => {
    const fs = e.clipboardData && e.clipboardData.files;
    if (fs && fs.length && isChat) {
      e.preventDefault();
      addFiles([...fs]);
    }
  };
  const rows = Math.min(10, Math.max(1, text.split("\n").length));
  const placeholder = s.concierge ? "What do you want to start?" : isChat ? "Message mado, or /new plan"
    : 'Message, or /new <machine> "<project>" [profile] [mode], or /new plan';

  return html`<form class="composer" onSubmit=${(e) => { e.preventDefault(); send(); }}>
    ${files.length > 0 && html`<div class="chips">${files.map((f) => html`<span key=${f.local} class=${cx("chip", !f.id && "loading")}>
      <span class="c-name">${f.name}</span><span class="muted"> ${f.id ? fmtBytes(f.size) : "uploading…"}</span>
      <button type="button" class="link" aria-label=${"remove " + f.name} onClick=${() => removeFile(f)}>×</button></span>`)}</div>`}
    <textarea ref=${ta} rows=${rows} value=${text} placeholder=${placeholder} aria-label="Message"
      onInput=${(e) => setText(e.currentTarget.value)} onKeyDown=${onKeyDown} onPaste=${onPaste}></textarea>
    <div class="c-bar">
      ${isChat && html`<button type="button" class="icon-btn" aria-label="Attach files" title="Attach files"
          onClick=${() => fileInput.current && fileInput.current.click()}><${IconClip} /></button>
        <input ref=${fileInput} type="file" multiple class="hidden-input"
          onChange=${(e) => { addFiles([...e.currentTarget.files]); e.currentTarget.value = ""; }} />`}
      ${isChat
        ? html`<select class="picker" aria-label="Model" value=${s.model} onChange=${(e) => onModel(e.currentTarget.value)}>
            ${chatModels.map((m) => html`<option key=${m.name} value=${m.name}>${m.label || m.name}</option>`)}
            ${!chatModels.some((m) => m.name === s.model) && html`<option value=${s.model}>${s.model}</option>`}
          </select>`
        : html`<span class="picker static" title="Permission mode (change it in the details panel)">${permissionLabel(s)}</span>`}
      <span class="spacer"></span>
      ${running && html`<button type="button" class="icon-btn stop" aria-label="Interrupt (Esc)" title="Interrupt (Esc)" onClick=${onInterrupt}><${IconStop} /></button>`}
      <button type="submit" class="send" aria-label="Send" title="Send (Enter)" disabled=${!canSend}><${IconSend} /></button>
    </div>
  </form>`;
}
