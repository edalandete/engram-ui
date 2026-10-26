// Inject a 14-day SVG sparkline into [data-sparkline] elements.
// Data source: /api/activity?days=14 (project-aware via cookie).

(function () {
  function buildSVG(values, w, h) {
    const max = Math.max(1, ...values);
    const stepX = w / Math.max(1, values.length - 1);
    const pts = values.map((v, i) => {
      const x = i * stepX;
      const y = h - (v / max) * (h - 2) - 1;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    });
    const lastX = (values.length - 1) * stepX;
    const lastY = h - (values[values.length - 1] / max) * (h - 2) - 1;
    return `
      <svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" xmlns="http://www.w3.org/2000/svg" class="spark-svg">
        <polyline points="${pts.join(" ")}" fill="none" stroke="currentColor" stroke-width="1.2"/>
        <circle cx="${lastX.toFixed(1)}" cy="${lastY.toFixed(1)}" r="2" fill="currentColor"/>
      </svg>`;
  }

  async function init() {
    const targets = document.querySelectorAll("[data-sparkline]");
    if (!targets.length) return;

    const section = document.getElementById("heatmap-section");
    const project = section ? section.dataset.project : "";
    // Same series the heatmap draws, last two weeks of it. Sharing the fetch
    // halves what the dashboard costs the server; see engramActivity in app.js.
    const series = await window.engramActivity(project);
    const data = series.slice(-14);
    if (!data.length) return;

    const values = data.map((d) => d.count);
    const total = values.reduce((a, b) => a + b, 0);

    targets.forEach((el) => {
      const kind = el.getAttribute("data-sparkline");
      el.innerHTML = buildSVG(values, 100, 22);
      const tip = el.parentElement.querySelector("[data-spark-tip]");
      if (tip) {
        const last7 = values.slice(-7).reduce((a, b) => a + b, 0);
        tip.textContent = `${last7} last 7d · ${total} last 14d`;
        tip.dataset.kind = kind;
      }
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => init().catch((e) => console.error("engram-ui: dashboard widget failed", e)));
  } else {
    init().catch((e) => console.error("engram-ui: dashboard widget failed", e));
  }
})();
