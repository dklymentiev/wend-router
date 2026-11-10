// The "?" help of a dashboard page: a side panel with that page's guide, and a button
// that copies the same guide for an agent to follow. Both read /help/<page>.md.
//
// wendHelp(button, page): page is a name, or a function that returns one (the Pro
// dashboard's help follows its open tab).
(() => {
  const esc = (s) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  // Enough Markdown for the help texts: headings, paragraphs, lists, tables, code,
  // bold, links. Everything is escaped first; only these marks become tags.
  function inline(s) {
    return esc(s)
      .replace(/`([^`]+)`/g, "<code>$1</code>")
      .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
      .replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+|\/[^)\s]*)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  }
  function render(md) {
    const lines = md.split("\n"), out = [];
    let i = 0;
    while (i < lines.length) {
      const l = lines[i];
      if (l.startsWith("```")) {
        const code = [];
        for (i++; i < lines.length && !lines[i].startsWith("```"); i++) code.push(lines[i]);
        out.push(`<pre><code>${esc(code.join("\n"))}</code></pre>`); i++; continue;
      }
      const h = /^(#{1,3}) (.*)/.exec(l);
      if (h) { out.push(`<h${h[1].length}>${inline(h[2])}</h${h[1].length}>`); i++; continue; }
      if (l.startsWith("|")) {
        const rows = [];
        for (; i < lines.length && lines[i].startsWith("|"); i++) {
          if (/^\|[\s|:-]+\|$/.test(lines[i])) continue; // the line under the header
          rows.push(lines[i].slice(1, -1).split("|").map((c) => inline(c.trim())));
        }
        const [head, ...body] = rows;
        out.push(`<table><thead><tr>${head.map((c) => `<th>${c}</th>`).join("")}</tr></thead><tbody>` +
          body.map((r) => `<tr>${r.map((c) => `<td>${c}</td>`).join("")}</tr>`).join("") + "</tbody></table>");
        continue;
      }
      const li = /^(- |\d+\. )/;
      if (li.test(l)) {
        const tag = l.startsWith("- ") ? "ul" : "ol", items = [];
        for (; i < lines.length && (li.test(lines[i]) || /^ {2,}\S/.test(lines[i])); i++) {
          if (li.test(lines[i])) items.push(lines[i].replace(li, ""));
          else items[items.length - 1] += " " + lines[i].trim();
        }
        out.push(`<${tag}>${items.map((t) => `<li>${inline(t)}</li>`).join("")}</${tag}>`);
        continue;
      }
      if (!l.trim()) { i++; continue; }
      const para = [];
      for (; i < lines.length && lines[i].trim() && !/^(#|```|\||- |\d+\. )/.test(lines[i]); i++) para.push(lines[i]);
      out.push(`<p>${inline(para.join(" "))}</p>`);
    }
    return out.join("");
  }

  let panel, current = "";
  function build() {
    panel = document.createElement("aside");
    panel.className = "sn-drawer";
    panel.id = "helpdr";
    panel.setAttribute("aria-label", "Help");
    panel.innerHTML = `<div class="sn-drawer__head"><h3>Help</h3>` +
      `<button class="sn-btn sn-btn--ghost" id="help-copy" title="copies this guide, with commands for this router, to paste into your agent">Copy for your agent</button>` +
      `<button class="sn-drawer__x" id="help-x" aria-label="Close">&times;</button></div>` +
      `<div class="sn-drawer__body sn-md" id="help-body"></div>`;
    document.body.append(panel);
    panel.querySelector("#help-x").onclick = close;
    panel.querySelector("#help-copy").onclick = async (e) => {
      const b = e.currentTarget;
      const text = `Set up Wend router running at ${location.origin}. Follow this guide; the commands in it already point at that address.\n\n` + current;
      try { await navigator.clipboard.writeText(text); b.textContent = "Copied"; }
      catch { b.textContent = "Copy failed: select the text instead"; }
      setTimeout(() => { b.textContent = "Copy for your agent"; }, 2000);
    };
    document.addEventListener("keydown", (e) => { if (e.key === "Escape") close(); });
    document.addEventListener("pointerdown", (e) => {
      if (panel.classList.contains("is-open") && !panel.contains(e.target) && !e.target.closest(".sn-help")) close();
    });
  }
  function close() { panel?.classList.remove("is-open"); }
  async function open(page) {
    if (!panel) build();
    const body = panel.querySelector("#help-body");
    try {
      const r = await fetch(`/help/${page}.md`);
      if (!r.ok) throw new Error(r.status);
      current = await r.text();
      body.innerHTML = render(current);
    } catch { current = ""; body.textContent = "The help could not be loaded."; }
    body.scrollTop = 0;
    panel.classList.add("is-open");
  }
  window.wendHelp = (button, page) => {
    button.onclick = () => open(typeof page === "function" ? page() : page);
  };
})();
