// DataBrain progressive enhancements.
// Every page works fully without this file. Nothing here talks to the server,
// and all text is inserted as text nodes (never innerHTML).
(() => {
  "use strict";

  const isMac = /Mac|iPhone|iPad/.test(navigator.platform || "");
  const el = (tag, cls, text) => {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text != null) node.textContent = text;
    return node;
  };

  // ---------------------------------------------------------------- SQL editor
  // Adds a title bar, a line-number gutter, a caret status line, Tab indentation
  // (press Esc first to Tab out of the editor), Ctrl/Cmd+S to save, and an
  // unsaved-changes guard.
  function setupEditor(area) {
    const wrap = el("div", "editor");
    const bar = el("div", "editor-bar");
    bar.setAttribute("aria-hidden", "true");
    bar.append(el("i"), el("i"), el("i"), el("b", null, "query.sql"), el("span", "dirty", "Unsaved"));
    const gutter = el("div", "editor-gutter");
    gutter.setAttribute("aria-hidden", "true");
    const status = el("div", "editor-status");
    status.setAttribute("aria-hidden", "true");
    const pos = el("span");
    const hint = el("span");
    const mod = isMac ? "⌘" : "Ctrl";
    hint.append(el("kbd", null, mod), " ", el("kbd", null, "S"), " save · ", el("kbd", null, "Esc"), " ", el("kbd", null, "Tab"), " leave editor");
    status.append(pos, hint);

    area.parentNode.insertBefore(wrap, area);
    wrap.append(bar, gutter, area);
    wrap.after(status);

    const original = area.value;
    let lastLines = 0;
    let escaped = false;
    let submitting = false;

    const render = () => {
      const lines = area.value.split("\n").length;
      if (lines !== lastLines) {
        let text = "";
        for (let i = 1; i <= lines; i++) text += i + "\n";
        gutter.textContent = text;
        lastLines = lines;
      }
      const before = area.value.slice(0, area.selectionStart);
      const ln = before.split("\n").length;
      const col = before.length - before.lastIndexOf("\n");
      pos.textContent = `Ln ${ln}, Col ${col} · ${lines} line${lines === 1 ? "" : "s"} · ${area.value.length} chars`;
      wrap.classList.toggle("is-dirty", area.value !== original);
      gutter.scrollTop = area.scrollTop;
    };

    const insert = (text) => {
      area.focus();
      // execCommand keeps the native undo stack; fall back if unsupported.
      if (!document.execCommand || !document.execCommand("insertText", false, text)) {
        area.setRangeText(text, area.selectionStart, area.selectionEnd, "end");
        area.dispatchEvent(new Event("input", { bubbles: true }));
      }
    };

    area.addEventListener("keydown", (e) => {
      if (e.key === "Escape") { escaped = true; return; }
      if (e.key === "Tab" && !escaped && !e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey) {
        e.preventDefault();
        insert("    ");
      }
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
        e.preventDefault();
        if (area.form) area.form.requestSubmit();
      }
      if (e.key !== "Tab") escaped = false;
    });
    ["input", "keyup", "click", "select", "scroll"].forEach((t) => area.addEventListener(t, render));
    if (area.form) area.form.addEventListener("submit", () => { submitting = true; });
    window.addEventListener("beforeunload", (e) => {
      if (!submitting && area.value !== original) { e.preventDefault(); e.returnValue = ""; }
    });
    render();
  }

  // ------------------------------------------------------- SQL highlighting
  const KEYWORDS = new Set((
    "add all alter and as asc begin between by cascade case check column comment commit constraint create cross " +
    "default deferrable delete desc distinct do drop else end exists extension false foreign from full function " +
    "grant group having if ilike in index inner insert intersect into is join key left like limit materialized not " +
    "null nulls offset on or order outer owner primary references rename replace restrict returning revoke right " +
    "rollback schema select sequence set table then to transaction trigger true truncate type union unique update " +
    "using values view when where with " +
    "bigint bigserial boolean bool bytea char date decimal double float int integer interval json jsonb numeric " +
    "real serial smallint text time timestamp timestamptz uuid varchar"
  ).split(" "));
  const TOKENS = /(--[^\n]*|\/\*[\s\S]*?\*\/)|('(?:[^']|'')*'?)|(\b\d+(?:\.\d+)?\b)|([A-Za-z_][A-Za-z0-9_]*)/g;

  function highlight(pre) {
    const src = pre.textContent;
    if (src.length > 200000) return;
    const out = document.createDocumentFragment();
    let last = 0;
    for (const m of src.matchAll(TOKENS)) {
      let cls = null;
      if (m[1]) cls = "tok-com";
      else if (m[2]) cls = "tok-str";
      else if (m[3]) cls = "tok-num";
      else if (m[4] && KEYWORDS.has(m[4].toLowerCase())) cls = "tok-kw";
      if (!cls) continue;
      if (m.index > last) out.append(src.slice(last, m.index));
      out.append(el("span", cls, m[0]));
      last = m.index + m[0].length;
    }
    if (last < src.length) out.append(src.slice(last));
    pre.replaceChildren(out);
  }

  document.querySelectorAll("textarea#sql").forEach(setupEditor);
  document.querySelectorAll("pre.sql").forEach(highlight);
})();
