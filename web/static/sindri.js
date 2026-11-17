// Progressive enhancement for the read-only UI. Every page works without it.
// No eval, no inline handlers, no innerHTML: text goes in through textContent.
(() => {
  const root = document.documentElement;
  root.classList.add("js-enhanced");
  const live = document.getElementById("live");

  // Copy: data-copy holds the text, data-copy-target names an element to copy.
  const copyText = async (text) => {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try { await navigator.clipboard.writeText(text); return true; } catch (e) { /* fall through */ }
    }
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed"; // CSSOM, not an inline style attribute
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch (e) { /* ok stays false */ }
    ta.remove();
    return ok;
  };
  document.addEventListener("click", async (ev) => {
    const btn = ev.target.closest("[data-copy], [data-copy-target]");
    if (!btn) return;
    const target = btn.dataset.copyTarget && document.getElementById(btn.dataset.copyTarget);
    const text = target ? target.textContent.trim() : btn.dataset.copy;
    if (text == null || !(await copyText(text))) return;
    const label = btn.dataset.label || btn.textContent;
    btn.dataset.label = label;
    btn.textContent = "Copied";
    if (live) live.textContent = (btn.getAttribute("aria-label") || "Text").replace(/^Copy /, "") + " copied";
    clearTimeout(btn.timer);
    btn.timer = setTimeout(() => {
      btn.textContent = label;
      if (live) live.textContent = "";
    }, 1500);
  });

  // Times: show the viewer's local time first, then the UTC instant.
  const fmt = new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit" });
  const localTimes = () => {
    document.querySelectorAll("time[datetime]").forEach((el) => {
      const d = new Date(el.getAttribute("datetime"));
      if (!isNaN(d)) el.title = fmt.format(d) + " (local) · " + el.getAttribute("datetime");
    });
  };
  localTimes();
  document.addEventListener("htmx:afterSwap", localTimes);

  // Keyboard: g then r goes to the repositories, ? lists the keys.
  const dialog = document.getElementById("keys");
  let armed = 0;
  document.addEventListener("keydown", (ev) => {
    if (ev.ctrlKey || ev.metaKey || ev.altKey) return;
    const el = ev.target;
    if (el.closest && el.closest("input, textarea, select, [contenteditable]:not([contenteditable=false])")) return;
    if (ev.key === "?" && dialog && dialog.showModal) {
      if (dialog.open) dialog.close(); else dialog.showModal();
    } else if (ev.key === "g") {
      armed = Date.now();
    } else if (ev.key === "r" && Date.now() - armed < 1000) {
      location.href = "/ui/";
    } else if (ev.key !== "Shift") {
      armed = 0;
    }
    // "/" is reserved for search, which comes later.
  });

  // A session that ended mid-page: send the browser to sign in, then back here.
  document.addEventListener("htmx:responseError", (ev) => {
    if (ev.detail.xhr && ev.detail.xhr.status === 401) {
      location.href = "/auth/login?next=" + encodeURIComponent(location.pathname + location.search);
    }
  });
})();
