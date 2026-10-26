// GitHub-style activity heatmap.
// Pulls /api/activity?days=182 and renders a 7×N grid (rows = weekday, cols = week).

(function () {
  function level(n) {
    if (n === 0) return 0;
    if (n <= 2) return 1;
    if (n <= 5) return 2;
    if (n <= 10) return 3;
    return 4;
  }

  async function init() {
    const root = document.getElementById("heatmap");
    if (!root) return;
    const section = document.getElementById("heatmap-section");
    const project = section ? section.dataset.project : "";

    // Shared with the sparklines: one request per dashboard render. See
    // engramActivity in app.js.
    const data = await window.engramActivity(project);
    if (!data.length) {
      root.innerHTML = `<p class="text-subtext">no activity data</p>`;
      return;
    }

    // Bucket days into weeks (Sunday-start).
    if (!data.length) {
      root.innerHTML = `<p class="text-subtext">No activity in the window.</p>`;
      return;
    }

    // Find leading offset so first column starts on Sunday.
    const firstDow = new Date(data[0].date + "T00:00:00Z").getUTCDay();
    const cells = new Array(firstDow).fill(null).concat(data);

    const cols = Math.ceil(cells.length / 7);
    const grid = document.createElement("div");
    grid.className = "hm-grid";
    grid.style.setProperty("--hm-cols", cols);

    for (let c = 0; c < cols; c++) {
      const col = document.createElement("div");
      col.className = "hm-col";
      for (let r = 0; r < 7; r++) {
        const day = cells[c * 7 + r];
        const cell = document.createElement("a");
        cell.className = "hm-cell";
        if (!day) {
          cell.classList.add("hm-empty");
        } else {
          cell.classList.add("hm-l" + level(day.count));
          cell.title = `${day.date} · ${day.count} obs`;
          cell.href = `/observations?since=${day.date}`;
        }
        col.appendChild(cell);
      }
      grid.appendChild(col);
    }
    root.innerHTML = "";
    root.appendChild(grid);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", () => init().catch((e) => console.error("engram-ui: dashboard widget failed", e)));
  } else {
    init().catch((e) => console.error("engram-ui: dashboard widget failed", e));
  }
})();
