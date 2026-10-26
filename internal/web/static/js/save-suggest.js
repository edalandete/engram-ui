// Live suggestions on /save form: while user types title or content,
// fetch /search?q=... and surface similar obs + reusable topic_keys.

(function () {
  function debounce(fn, ms) {
    let t = null;
    return (...args) => {
      clearTimeout(t);
      t = setTimeout(() => fn(...args), ms);
    };
  }

  function escapeHTML(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }

  function init() {
    const titleEl = document.querySelector('input[name="title"]');
    const contentEl = document.querySelector('textarea[name="content"]');
    const topicEl = document.querySelector('input[name="topic_key"]');
    const projectEl = document.querySelector('input[name="project"]');
    const panel = document.getElementById("save-suggest");
    if (!titleEl || !panel) return;

    async function refresh() {
      const q = (titleEl.value.trim() + " " + (contentEl ? contentEl.value.trim().slice(0, 200) : "")).trim();
      if (q.length < 4) {
        panel.innerHTML = "";
        return;
      }
      const params = new URLSearchParams({ q, limit: "8" });
      if (projectEl && projectEl.value.trim()) params.set("project", projectEl.value.trim());

      let res;
      try {
        const r = await fetch("/api/search-suggest?" + params.toString(), { credentials: "same-origin" });
        if (!r.ok) return;
        res = await r.json();
      } catch (e) { return; }

      if (!res || !res.length) {
        panel.innerHTML = `<p class="text-subtext">No similar observations.</p>`;
        return;
      }

      const topics = new Map();
      res.forEach((o) => {
        if (o.topic_key) topics.set(o.topic_key, (topics.get(o.topic_key) || 0) + 1);
      });

      let html = `<h4>Similar observations <span class="text-subtext">(${res.length})</span></h4><ul class="list">`;
      res.forEach((o) => {
        html += `<li class="list-row small">
          <a href="/observations/${o.id}" target="_blank" rel="noopener">
            <span class="meta-id">#${o.id}</span>
            <span class="badge">${escapeHTML(o.type)}</span>
            <span class="row-title">${escapeHTML(o.title)}</span>
            ${o.topic_key ? `<code class="text-peach">${escapeHTML(o.topic_key)}</code>` : ""}
          </a>
        </li>`;
      });
      html += `</ul>`;

      if (topics.size) {
        html += `<h4>Reuse topic-key</h4><div class="topic-suggest">`;
        Array.from(topics.entries())
          .sort((a, b) => b[1] - a[1])
          .slice(0, 5)
          .forEach(([k, n]) => {
            html += `<button class="action-btn" type="button" data-set-topic="${escapeHTML(k)}">${escapeHTML(k)} <span class="text-subtext">${n}</span></button>`;
          });
        html += `</div>`;
      }

      panel.innerHTML = html;
      panel.querySelectorAll("[data-set-topic]").forEach((btn) => {
        btn.addEventListener("click", () => {
          if (topicEl) topicEl.value = btn.getAttribute("data-set-topic");
        });
      });
    }

    const debounced = debounce(refresh, 350);
    titleEl.addEventListener("input", debounced);
    if (contentEl) contentEl.addEventListener("input", debounced);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
