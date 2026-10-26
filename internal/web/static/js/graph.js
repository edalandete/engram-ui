// Force-directed graph powered by cytoscape.js.
// Data: GET /api/graph?project=... → { nodes, edges, stats }.
// Honors the project cookie via the same baseURL the page already uses.

(function () {
  // Every toolbar lookup goes through this. They used to be bare
  // getElementById().addEventListener() chains, so reordering graph.html.tmpl
  // would throw a TypeError that aborted the rest of init and left the graph
  // half-wired with no error anyone would connect to the cause.
  const el = (id) => document.getElementById(id);
  // Colours come from the stylesheet's custom properties instead of a copy of
  // the palette. The copy was theme-blind: the graph kept painting dark-theme
  // hexes on the light background, and every palette change had to be made
  // twice. Read once at build time — a theme flip re-renders this page anyway,
  // since it is the one screen that navigates without htmx.
  function paint(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || "#888888";
  }

  function palette() {
    const p = {
      lavender: paint("--lavender"),
      subtext:  paint("--subtext"),
      overlay:  paint("--overlay"),
    };
    return {
      relation: {
        related:        paint("--teal"),
        compatible:     paint("--teal"),
        scoped:         paint("--peach"),
        conflicts_with: paint("--red"),
        supersedes:     p.lavender,
        not_conflict:   p.overlay,
        pending:        p.subtext,
      },
      type: {
        decision:        p.lavender,
        architecture:    paint("--blue"),
        bugfix:          paint("--red"),
        pattern:         paint("--green"),
        config:          paint("--peach"),
        discovery:       paint("--mauve"),
        manual:          p.subtext,
        session_summary: paint("--teal"),
        missing:         p.overlay,
      },
      fallbackType:     p.lavender,
      fallbackRelation: p.subtext,
    };
  }

  const COLORS = palette();
  function colorForType(t) { return COLORS.type[t] || COLORS.fallbackType; }
  function colorForRelation(r) { return COLORS.relation[r] || COLORS.fallbackRelation; }

  function buildElements(payload) {
    const els = [];
    for (const n of payload.nodes) {
      els.push({
        group: "nodes",
        data: { id: n.id, label: n.label, type: n.type, obsId: n.obs_id, project: n.project },
      });
    }
    for (const e of payload.edges) {
      els.push({
        group: "edges",
        data: { id: e.id, source: e.source, target: e.target, relation: e.relation, status: e.status },
      });
    }
    return els;
  }

  function renderSelection(el) {
    const side = el("graph-side");
    if (!side) return;
    if (!el) {
      side.innerHTML = `<h3 class="text-subtext">Selection</h3><p class="text-subtext">Click a node or edge.</p>`;
      return;
    }
    if (el.isNode()) {
      const d = el.data();
      const url = d.obsId ? `/observations/${d.obsId}` : "#";
      side.innerHTML = `
        <h3>${escapeHTML(d.label || d.id)}</h3>
        <p>
          <span class="badge">${escapeHTML(d.type || "?")}</span>
          ${d.project ? `<span class="text-subtext">${escapeHTML(d.project)}</span>` : ""}
        </p>
        <p class="text-subtext"><code>${escapeHTML(d.id)}</code></p>
        <p>
          <a class="action-btn" href="${url}">→ Open</a>
          ${d.obsId ? `<a class="action-btn" href="/timeline/${d.obsId}">→ Timeline</a>` : ""}
        </p>
        <h4 class="text-subtext">Connections</h4>
        <ul class="list">
          ${el.connectedEdges().map(edge => {
            const other = edge.source().id() === el.id() ? edge.target() : edge.source();
            return `<li class="list-row small">
              <span class="badge" style="background:${colorForRelation(edge.data("relation"))}">${escapeHTML(edge.data("relation"))}</span>
              <a href="#" data-node-id="${escapeHTML(other.id())}">${escapeHTML(other.data("label") || other.id())}</a>
            </li>`;
          }).join("")}
        </ul>
      `;
    } else {
      const d = el.data();
      side.innerHTML = `
        <h3>Edge</h3>
        <p><span class="badge" style="background:${colorForRelation(d.relation)}">${escapeHTML(d.relation)}</span>
        <span class="badge status-${escapeHTML(d.status)}">${escapeHTML(d.status)}</span></p>
        <p class="text-subtext">source <code>${escapeHTML(d.source)}</code></p>
        <p class="text-subtext">target <code>${escapeHTML(d.target)}</code></p>
      `;
    }
  }

  function escapeHTML(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }


  async function init() {    const container = document.getElementById("cy");
    if (!container || typeof cytoscape === "undefined") return;

    // Run once per container. Navigating away and pressing Back makes htmx
    // restore this page from its cache and re-execute the <script> tags in it,
    // which would build a second cytoscape instance on top of the first —
    // 373 KB of library and a graph whose filters only reach half the nodes.
    if (container.dataset.cyInit) return;
    container.dataset.cyInit = "1";

    const stats = document.getElementById("graph-stats");
    if (stats) stats.textContent = "loading…";

    const params = new URLSearchParams(window.location.search);
    const family = params.get("family") || "";

    let payload;
    try {
      const res = await fetch("/api/graph" + (family ? "?family=" + encodeURIComponent(family) : ""), { credentials: "same-origin" });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      payload = await res.json();
    } catch (err) {
      stats.textContent = "load failed: " + err.message;
      return;
    }

    // Populate family dropdown.
    const famSel = el("family-select");
    if (famSel && Array.isArray(payload.families)) {
      famSel.innerHTML = `<option value="">all (${payload.families.length})</option>`;
      payload.families.forEach((f) => {
        const opt = document.createElement("option");
        opt.value = f;
        opt.textContent = f;
        if (f === family) opt.selected = true;
        famSel.appendChild(opt);
      });
      famSel.addEventListener("change", () => {
        const next = new URLSearchParams(window.location.search);
        if (famSel.value) next.set("family", famSel.value);
        else next.delete("family");
        window.location.search = next.toString();
      });
    }

    stats.textContent = `${payload.stats.observations} obs · ${payload.stats.relations} edges${payload.project ? ` · ${payload.project}` : ""}${family ? ` · family: ${family}` : ""}`;

    const cy = cytoscape({
      container,
      elements: buildElements(payload),
      wheelSensitivity: 0.3,
      style: [
        {
          selector: "node",
          style: {
            "background-color": (ele) => colorForType(ele.data("type")),
            label: "data(label)",
            color: paint("--text"),
            "font-size": 10,
            "text-valign": "bottom",
            "text-margin-y": 4,
            "text-wrap": "ellipsis",
            "text-max-width": 120,
            width: 16,
            height: 16,
            "border-width": 1,
            "border-color": paint("--surface"),
          },
        },
        {
          selector: "node:selected",
          style: { "border-color": paint("--lavender"), "border-width": 3, "z-index": 99 },
        },
        {
          selector: "node.faded",
          style: { opacity: 0.15 },
        },
        {
          selector: "edge",
          style: {
            width: 1.5,
            "line-color": (ele) => colorForRelation(ele.data("relation")),
            "target-arrow-color": (ele) => colorForRelation(ele.data("relation")),
            "target-arrow-shape": "triangle",
            "curve-style": "bezier",
            "line-style": (ele) => (ele.data("status") === "pending" || ele.data("relation") === "pending") ? "dashed" : "solid",
            opacity: 0.7,
          },
        },
        {
          selector: "edge:selected",
          style: { width: 3, opacity: 1, "z-index": 99 },
        },
        {
          selector: "edge.faded",
          style: { opacity: 0.05 },
        },
      ],
      layout: { name: "cose", animate: false, idealEdgeLength: 80, nodeRepulsion: 8000 },
    });

    cy.on("tap", "node", (evt) => {
      const node = evt.target;
      cy.elements().addClass("faded");
      node.removeClass("faded");
      node.neighborhood().removeClass("faded");
      renderSelection(node);
    });
    cy.on("tap", "edge", (evt) => {
      cy.elements().addClass("faded");
      evt.target.removeClass("faded");
      evt.target.source().removeClass("faded");
      evt.target.target().removeClass("faded");
      renderSelection(evt.target);
    });
    cy.on("tap", (evt) => {
      if (evt.target === cy) {
        cy.elements().removeClass("faded");
        renderSelection(null);
      }
    });
    cy.on("dblclick", "node", (evt) => {
      const id = evt.target.data("obsId");
      if (id) window.location.href = `/observations/${id}`;
    });

    // Toolbar wiring
    el("layout-select")?.addEventListener("change", (e) => {
      cy.layout({ name: e.target.value, animate: false }).run();
    });
    el("graph-fit")?.addEventListener("click", () => cy.fit(null, 30));
    el("graph-reset")?.addEventListener("click", () => {
      cy.elements().removeClass("faded");
      cy.fit(null, 30);
      renderSelection(null);
    });
    el("hide-orphans")?.addEventListener("change", (e) => {
      cy.nodes().forEach((n) => {
        if (n.degree(false) === 0) n.style("display", e.target.checked ? "none" : "element");
      });
    });
    const searchInput = el("graph-search");
    searchInput.addEventListener("input", () => {
      const q = searchInput.value.trim().toLowerCase();
      if (!q) {
        cy.elements().removeClass("faded");
        return;
      }
      cy.elements().addClass("faded");
      cy.nodes().forEach((n) => {
        const label = (n.data("label") || "").toLowerCase();
        if (label.includes(q)) {
          n.removeClass("faded");
          n.neighborhood().removeClass("faded");
        }
      });
    });

    // Side-panel link → focus that node.
    el("graph-side")?.addEventListener("click", (e) => {
      const a = e.target.closest("a[data-node-id]");
      if (!a) return;
      e.preventDefault();
      const node = cy.getElementById(a.getAttribute("data-node-id"));
      if (node && node.length) {
        cy.elements().addClass("faded");
        node.removeClass("faded");
        node.neighborhood().removeClass("faded");
        cy.center(node);
        renderSelection(node);
      }
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
