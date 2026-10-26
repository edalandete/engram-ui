// engram-ui — vim-style keybinds + clipboard helper.
// j/k     move row focus through .list-row
// Enter   open focused row
// /       focus the first search input
// g g     jump to top
// G       jump to bottom
// ?       toggle keybind help
//
// Skipped while typing in inputs/textareas.

(function () {
  let rows = [];
  let idx = -1;

  function refreshRows() {
    rows = Array.from(document.querySelectorAll(".list-row"));
    if (idx >= rows.length) idx = rows.length - 1;
  }

  function focus(i) {
    if (rows.length === 0) return;
    idx = Math.max(0, Math.min(rows.length - 1, i));
    rows.forEach((r) => r.classList.remove("focused"));
    const row = rows[idx];
    row.classList.add("focused");
    row.scrollIntoView({ block: "nearest", inline: "nearest" });
  }

  function inEditable(el) {
    if (!el) return false;
    const tag = el.tagName;
    return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || el.isContentEditable;
  }

  let lastG = 0;

  document.addEventListener("keydown", (e) => {
    if (inEditable(e.target)) {
      // allow Esc to blur
      if (e.key === "Escape") e.target.blur();
      return;
    }
    if (e.metaKey || e.ctrlKey || e.altKey) return;

    // Escape closes the help panel. It used to live inside the branch above,
    // reachable only while typing in a field — but with the panel open the
    // focus is on <body>, so the shortcut the panel itself advertises did
    // nothing.
    if (e.key === "Escape") {
      const panel = document.getElementById("kbd-help");
      if (panel) panel.remove();
      return;
    }

    // Only the keys that move the cursor need the row list. This used to run
    // on every keydown, so a 500-row list did a querySelectorAll and rebuilt
    // an array for keys that do nothing at all.
    if (e.key === "j" || e.key === "k" || e.key === "Enter" || e.key === "G" || e.key === "g") {
      refreshRows();
    }

    switch (e.key) {
      case "j":
        e.preventDefault();
        focus(idx < 0 ? 0 : idx + 1);
        break;
      case "k":
        e.preventDefault();
        focus(idx <= 0 ? 0 : idx - 1);
        break;
      case "Enter":
        if (idx >= 0 && rows[idx]) {
          const link = rows[idx].querySelector("a[href]");
          if (link) {
            e.preventDefault();
            window.location.href = link.href;
          }
        }
        break;
      case "/":
        e.preventDefault();
        const search = document.querySelector('input[type="search"], input[name="q"]');
        if (search) {
          search.focus();
          search.select();
        }
        break;
      case "g":
        if (Date.now() - lastG < 500) {
          e.preventDefault();
          window.scrollTo(0, 0);
          focus(0);
          lastG = 0;
        } else {
          lastG = Date.now();
        }
        break;
      case "G":
        e.preventDefault();
        window.scrollTo(0, document.body.scrollHeight);
        focus(rows.length - 1);
        break;
      case "?":
        e.preventDefault();
        toggleHelp();
        break;
    }
  });

  function toggleHelp() {
    let panel = document.getElementById("kbd-help");
    if (panel) {
      panel.remove();
      return;
    }
    panel = document.createElement("div");
    panel.id = "kbd-help";
    panel.innerHTML = `
      <div class="kbd-help-card">
        <h3>Keybinds</h3>
        <table>
          <tr><td><kbd>j</kbd> / <kbd>k</kbd></td><td>next / prev row</td></tr>
          <tr><td><kbd>Enter</kbd></td><td>open focused row</td></tr>
          <tr><td><kbd>/</kbd></td><td>focus search</td></tr>
          <tr><td><kbd>g g</kbd></td><td>jump to top</td></tr>
          <tr><td><kbd>G</kbd></td><td>jump to bottom</td></tr>
          <tr><td><kbd>?</kbd></td><td>toggle this panel</td></tr>
          <tr><td><kbd>Esc</kbd></td><td>blur input / close panel</td></tr>
        </table>
        <p class="text-subtext">Click anywhere to dismiss.</p>
      </div>`;
    document.body.appendChild(panel);
    panel.addEventListener("click", () => panel.remove());
  }

  // ── Sticky filters (data-sticky attr on selects) ─────────────────────────
  const STICKY_KEY = "engram.search_filters";
  function loadSticky() {
    try { return JSON.parse(localStorage.getItem(STICKY_KEY) || "{}"); } catch (e) { return {}; }
  }
  function saveSticky(obj) { localStorage.setItem(STICKY_KEY, JSON.stringify(obj)); }

  function applyStickyOnLoad() {
    const fields = document.querySelectorAll("[data-sticky]");
    if (fields.length === 0) return;
    const stored = loadSticky();
    const params = new URLSearchParams(window.location.search);
    fields.forEach((field) => {
      const key = field.getAttribute("data-sticky");
      // Don't override if user explicitly passed the param this navigation.
      if (params.has(key)) return;
      if (stored[key] != null && stored[key] !== "") {
        field.value = stored[key];
      }
    });
  }

  document.addEventListener("submit", (e) => {
    const form = e.target;
    if (!form || form.tagName !== "FORM") return;
    const fields = form.querySelectorAll("[data-sticky]");
    if (fields.length === 0) return;
    const stored = loadSticky();
    fields.forEach((field) => {
      stored[field.getAttribute("data-sticky")] = field.value || "";
    });
    saveSticky(stored);
  });

  document.addEventListener("DOMContentLoaded", applyStickyOnLoad);
  document.addEventListener("htmx:afterSwap", applyStickyOnLoad);

  // ── Bulk action bar ───────────────────────────────────────────────────────
  function refreshBulkBar() {
    const bar = document.querySelector("[data-bulk-bar]");
    if (!bar) return;
    const checked = document.querySelectorAll(".bulk-check:checked");
    const counter = bar.querySelector("[data-bulk-count]");
    if (counter) counter.textContent = checked.length;
    bar.classList.toggle("active", checked.length > 0);
  }

  document.addEventListener("change", (e) => {
    if (e.target.matches(".bulk-check")) refreshBulkBar();
  });
  document.addEventListener("click", (e) => {
    if (e.target.matches("[data-bulk-clear]")) {
      e.preventDefault();
      document.querySelectorAll(".bulk-check:checked").forEach((c) => (c.checked = false));
      refreshBulkBar();
    }
  });
  // Initial render after DOM ready (htmx swap also fires this).
  document.addEventListener("DOMContentLoaded", refreshBulkBar);
  document.addEventListener("htmx:afterSwap", refreshBulkBar);

  // ── Theme toggle (client-side; hx-boost only swaps <main>) ────────────────
  // The toggle must flip data-theme on <html>, which htmx navigation never
  // re-renders. Do it in JS and persist via cookie so reloads + server renders
  // (data-theme="{{.Theme}}") stay consistent.
  function applyTheme(theme) {
    document.documentElement.setAttribute("data-theme", theme);
    document.cookie =
      "engram_theme=" + theme + ";path=/;max-age=" + 60 * 60 * 24 * 365 + ";samesite=lax";
    const icon = document.querySelector("[data-theme-icon]");
    if (icon) icon.textContent = theme === "dark" ? "☾" : "☀";
  }
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-theme-toggle]");
    if (!btn) return;
    e.preventDefault();
    const cur = document.documentElement.getAttribute("data-theme");
    applyTheme(cur === "dark" ? "light" : "dark");
  });

  // ── Active nav link (data-theme swaps main only; recompute on swap) ───────
  function markActiveNav() {
    const path = window.location.pathname;
    document.querySelectorAll(".sidebar nav a").forEach((a) => {
      const href = a.getAttribute("href");
      const on = href === "/" ? path === "/" : path === href || path.startsWith(href + "/");
      a.classList.toggle("active", on);
    });
  }
  document.addEventListener("DOMContentLoaded", markActiveNav);
  document.addEventListener("htmx:afterSwap", markActiveNav);

  // ── Clipboard buttons (data-clipboard-target="#id") ───────────────────────
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-clipboard-target]");
    if (!btn) return;
    e.preventDefault();
    const sel = btn.getAttribute("data-clipboard-target");
    const node = document.querySelector(sel);
    if (!node) return;
    const text = node.innerText || node.textContent || "";
    // The Clipboard API only exists in a secure context. Loopback counts as
    // one, but ENGRAM_UI_LISTEN can be pointed at 0.0.0.0 and reached over
    // plain http from another machine — where navigator.clipboard is undefined
    // and this would be a TypeError, not the handled rejection below.
    if (!navigator.clipboard) {
      btn.textContent = "✗ needs https";
      return;
    }
    navigator.clipboard.writeText(text).then(
      () => {
        const old = btn.textContent;
        btn.textContent = "✓ copied";
        setTimeout(() => (btn.textContent = old), 1200);
      },
      () => {
        btn.textContent = "✗ failed";
      }
    );
  });

  // ── one activity fetch per dashboard render ───────────────────────────────
  // The heatmap wanted 182 days and the sparklines wanted 14, so the dashboard
  // called /api/activity twice and the server scanned 2000 observations twice
  // to draw the same series. Fetch the long one once and let the short one
  // slice it. Cleared on every swap, or a project switch would redraw with the
  // previous project's numbers.
  const activityCache = new Map();
  window.engramActivity = function (project) {
    const key = project || "";
    if (!activityCache.has(key)) {
      const url =
        "/api/activity?days=182" + (project ? "&project=" + encodeURIComponent(project) : "");
      activityCache.set(
        key,
        fetch(url, { credentials: "same-origin" })
          .then((r) => (r.ok ? r.json() : []))
          .then((d) => (Array.isArray(d) ? d : []))
          .catch(() => [])
      );
    }
    return activityCache.get(key);
  };
  document.addEventListener("htmx:beforeSwap", () => activityCache.clear());

  // ── show error pages instead of swallowing them ───────────────────────────
  // The body is hx-boost="true", so every link and form goes through htmx, and
  // htmx refuses to swap any response with a status >= 400. renderError sends
  // a fully rendered error page with a 500, which htmx then threw away: the
  // click appeared to do nothing at all. Not one screen — every failure path
  // in the app, from a bad search query to a failed publish.
  //
  // Fixed here rather than by answering 200, because the status is the honest
  // one and this keeps it: the server still says 500, the user still sees why.
  document.addEventListener("htmx:beforeSwap", (e) => {
    if (e.detail.xhr.status >= 400) {
      e.detail.shouldSwap = true;
      e.detail.isError = false;
    }
  });
})();
