// Package handlers wires HTTP routes for the engram-ui web server.
//
// Each handler takes data ONLY from internal/client (never net/http directly
// against engram) and renders via html/template.
package handlers

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	gmhtml "github.com/yuin/goldmark/renderer/html"

	"github.com/edalandete/engram-ui/internal/client"
	"github.com/edalandete/engram-ui/internal/cloudbridge"
	"github.com/edalandete/engram-ui/internal/cloudclient"
	"github.com/edalandete/engram-ui/internal/engramcli"
	"github.com/edalandete/engram-ui/internal/gitbridge"
	"github.com/edalandete/engram-ui/internal/version"
	"github.com/edalandete/engram-ui/internal/web"
)

var markdownRenderer = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(gmhtml.WithHardWraps()),
)

func renderMarkdown(s string) template.HTML {
	var buf bytes.Buffer
	if err := markdownRenderer.Convert([]byte(s), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(s))
	}
	return template.HTML(buf.String())
}

const (
	projectCookie = "engram_project"
	themeCookie   = "engram_theme"

	// Ceilings for every number that arrives in a query string. Two of these
	// reach make() as a capacity, so without a roof `?days=500000000` is an
	// out-of-memory crash from a URL — on a server with no authentication.
	// Set well above any real use: a page nobody reads past 200 rows, a scan
	// wider than any project, and five years of heatmap.
	maxPageLimit   = 200
	maxScanLimit   = 20000
	maxHeatmapDays = 1830
)

var canonicalTypes = []string{"decision", "architecture", "bugfix", "pattern", "config", "discovery", "manual", "session_summary"}
var canonicalScopes = []string{"project", "personal"}
var canonicalRelations = []string{"related", "compatible", "scoped", "conflicts_with", "supersedes", "not_conflict"}

type Server struct {
	C          *client.Client
	CLI        *engramcli.Runner
	Bridge     *cloudbridge.Bridge
	Share      *gitbridge.Repos
	cloudToken string
	// tpl is the base (layout + pagination); pages holds one fully parsed set
	// per screen, built once in New. Both are read-only after construction, so
	// concurrent requests share them without a lock.
	tpl   *template.Template
	pages map[string]*template.Template

	authorsMu    sync.Mutex
	authorsCache map[string]authorEntry // key: project ("" = all)

	pullUsersMu    sync.Mutex
	pullUsersCache map[string]pullUsersEntry // key: project

	// One scan slot for the whole process. See scanJob.
	scanMu sync.Mutex
	scan   *scanJob
}

// scanJob is the state of the conflict scan — the one operation here that runs
// for minutes rather than milliseconds.
//
// It used to run inside the POST handler, which was wrong in three ways at
// once: the browser sat on a blank navigation for the whole run with nothing
// to show it had started; navigating away dropped the request and the result
// with it — engram's ScanProject takes no context, so it scanned to the end and
// wrote everything with nobody left to report to; and nothing stopped a
// second, third or fourth scan from being launched on top of the first, all of
// them queueing on engram's single database connection.
//
// So the handler starts a goroutine and returns immediately, and this holds
// what the screen needs afterwards. Deliberately in memory and deliberately
// global: it survives navigation (which is what was asked for) and not a
// restart. A managed engram dies with engram-ui and the scan with it; an
// external one finishes the scan unseen, which is no worse than before. One slot for
// every project rather than one per project — two concurrent scans is exactly
// the thing worth preventing.
type scanJob struct {
	Project string
	Started time.Time
	Ended   time.Time // zero while running
	Done    bool
	Result  *client.ScanResult
	Err     string
}

// Elapsed is how long the scan has been running, or how long it took.
func (j *scanJob) Elapsed() string {
	end := j.Ended
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(j.Started).Round(time.Second).String()
}

// claimScan reserves the scan slot, reporting false when one is already
// running. Claim and start are one atomic step on purpose: checking first and
// starting after leaves a window for two double-clicks to both get through.
func (s *Server) claimScan(project string) bool {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.scan != nil && !s.scan.Done {
		return false
	}
	s.scan = &scanJob{Project: project, Started: time.Now()}
	return true
}

func (s *Server) finishScan(res *client.ScanResult, err error) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.scan == nil {
		return
	}
	s.scan.Ended = time.Now()
	s.scan.Result = res
	s.scan.Done = true
	if err != nil {
		s.scan.Err = err.Error()
	}
	// Only our wait has a deadline (client.ScanConflicts), not engram's scan:
	// it takes no context and runs on. The slot is free again from here, so
	// say so before someone starts a second scan on top of it.
	if errors.Is(err, context.DeadlineExceeded) {
		s.scan.Err += " — engram-ui stopped waiting, but engram is still scanning and will write its results. Give it time before scanning again."
	}
}

// scanState returns a copy for the templates to read: the live job is written
// by the scan goroutine, and a template rendering the pointer would race it.
// The ScanResult it points at is only ever written before Done.
func (s *Server) scanState() *scanJob {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	if s.scan == nil {
		return nil
	}
	cp := *s.scan
	return &cp
}

// authorEntry caches a project's sync_id→pusher map derived from cloud chunks.
type authorEntry struct {
	index   map[string]string
	fetched time.Time
}

// pullUsersEntry caches the per-contributor chunk/obs counts for the /sync
// selective-pull dropdown. Computing it downloads every cloud chunk, so it is
// cached just like the author index.
type pullUsersEntry struct {
	users   []cloudbridge.UserCount
	fetched time.Time
}

// authorsTTL bounds how long a cached author index is reused before a refresh.
// Provenance changes only when someone pushes, so a few minutes is plenty;
// pulls invalidate explicitly via invalidateAuthors.
const authorsTTL = 5 * time.Minute

// pullUsersTTL bounds reuse of the cached selective-pull contributor list. Same
// reasoning as authorsTTL: it only changes on push/pull, which invalidate it.
const pullUsersTTL = 5 * time.Minute

func New(c *client.Client, cli *engramcli.Runner, bridge *cloudbridge.Bridge, share *gitbridge.Repos, cloudToken string) *Server {
	funcs := template.FuncMap{
		"snippet":      snippet,
		"add":          func(a, b int) int { return a + b },
		"sub":          func(a, b int) int { return a - b },
		"markdown":     renderMarkdown,
		"mdsnippet":    func(s string, n int) template.HTML { return renderMarkdown(snippet(s, n)) },
		"lookup":       lookup,
		"statusLabel":  statusLabel,
		"statusHelp":   statusHelp,
		"relationHelp": relationHelp,
	}
	tpl := template.Must(template.New("").Funcs(funcs).ParseFS(web.TemplatesFS,
		"templates/layout.html.tmpl",
		"templates/_pagination.html.tmpl",
	))
	return &Server{
		C: c, CLI: cli, Bridge: bridge, Share: share, cloudToken: cloudToken,
		tpl:            tpl,
		pages:          parsePages(tpl),
		authorsCache:   map[string]authorEntry{},
		pullUsersCache: map[string]pullUsersEntry{},
	}
}

// parsePages pre-builds one template set per page, once.
//
// Every content template defines a block called "content", so they cannot all
// live in a single set — they would collide. The way out was to clone the base
// and parse one page on every single request, which is a full template parse
// per page view. Doing it here instead costs one pass at startup and makes a
// broken template a boot failure rather than a 500 the first time someone
// happens to open that screen.
func parsePages(base *template.Template) map[string]*template.Template {
	names, err := fs.Glob(web.TemplatesFS, "templates/*.html.tmpl")
	if err != nil {
		panic("glob templates: " + err.Error())
	}
	out := make(map[string]*template.Template, len(names))
	for _, name := range names {
		short := strings.TrimPrefix(name, "templates/")
		if short == "layout.html.tmpl" || short == "_pagination.html.tmpl" {
			continue // already in the base
		}
		clone, err := base.Clone()
		if err != nil {
			panic("clone base template: " + err.Error())
		}
		if _, err := clone.ParseFS(web.TemplatesFS, name); err != nil {
			panic("parse " + short + ": " + err.Error())
		}
		out[short] = clone
	}
	return out
}

// agentCLI reports which LLM runner the semantic scan will use, or "" when it
// would fail.
//
// Read from our own environment rather than asked of engram, because engram has
// no endpoint for it and because the launcher starts engram as a child: it sees
// exactly the variables we see. The one case that lies is an engram started by
// hand with a different environment, which is also the case where the UI cannot
// know anything.
//
// Only the two names engram's factory accepts count as configured. Anything
// else would fail the same way, just later and with a worse message.
func agentCLI() string {
	switch v := strings.TrimSpace(os.Getenv("ENGRAM_AGENT_CLI")); v {
	case "claude", "opencode":
		return v
	}
	return ""
}

// The Conflicts screen speaks engram's vocabulary — judgment statuses and
// relation verbs — which is precise and completely opaque to anyone who has not
// read engram's source. These three turn each term into something a person can
// act on. Kept as template funcs rather than baked into the template so the
// wording lives in one place and the loops stay data-driven.
//
// Anything unknown falls through unchanged: a status or verb added by a newer
// engram shows up as its raw name rather than disappearing.

func statusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending review"
	case "judged":
		return "Judged"
	case "orphaned":
		return "Orphaned"
	case "ignored":
		return "Ignored"
	}
	return status
}

func statusHelp(status string) string {
	switch status {
	case "pending":
		return "Proposed links nobody has judged yet. These are what block publishing."
	case "judged":
		return "A verdict has been recorded, by you or by the LLM."
	case "orphaned":
		return "One of the two observations no longer exists, so the link means nothing."
	case "ignored":
		return "Dismissed on purpose. Kept so the same pair is not proposed again."
	}
	return status
}

func relationHelp(relation string) string {
	switch relation {
	case "related":
		return "Same area. Both are worth keeping."
	case "compatible":
		return "Both true at the same time."
	case "scoped":
		return "Both true, in different contexts."
	case "conflicts_with":
		return "They contradict each other — a real conflict, decide which one holds."
	case "supersedes":
		return "The newer one replaces the older."
	case "not_conflict":
		return "Looked similar, turned out to be a false alarm."
	}
	return relation
}

// lookup is a nil-safe map accessor for templates: {{lookup .Authors .SyncID}}.
func lookup(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /", s.dashboard)
	mux.HandleFunc("GET /search", s.searchRedirect)
	mux.HandleFunc("GET /observations", s.observations)
	mux.HandleFunc("GET /observations/{id}", s.observationDetail)
	mux.HandleFunc("GET /observations/{id}/edit", s.observationEdit)
	mux.HandleFunc("POST /observations/{id}/edit", s.observationEditSubmit)
	mux.HandleFunc("POST /observations/{id}/delete", s.observationDelete)
	mux.HandleFunc("POST /observations/bulk", s.observationsBulk)
	mux.HandleFunc("GET /timeline/{id}", s.timeline)
	mux.HandleFunc("GET /sessions", s.recentSessions)
	mux.HandleFunc("GET /sessions/{id}", s.sessionDetail)
	mux.HandleFunc("GET /conflicts", s.conflicts)
	mux.HandleFunc("GET /conflicts/{id}", s.conflictDetail)
	mux.HandleFunc("POST /conflicts/{id}/judge", s.judgeConflict)
	mux.HandleFunc("POST /conflicts/scan", s.scanConflicts)
	mux.HandleFunc("GET /conflicts/scan/status", s.scanStatus)
	mux.HandleFunc("GET /graph", s.graphPage)
	mux.HandleFunc("GET /api/graph", s.graphData)
	mux.HandleFunc("GET /api/activity", s.activityData)
	mux.HandleFunc("GET /api/search-suggest", s.searchSuggest)
	mux.HandleFunc("GET /set-project", s.setProject)
	mux.HandleFunc("GET /projects", s.projects)
	mux.HandleFunc("GET /prompts", s.recentPrompts)
	mux.HandleFunc("GET /topics", s.topics)
	mux.HandleFunc("GET /topics/{key...}", s.topicDetail)
	mux.HandleFunc("GET /save", s.savePage)
	mux.HandleFunc("POST /save", s.saveSubmit)
	mux.HandleFunc("GET /backup", s.backupSnapshot)
	mux.HandleFunc("GET /export.json", s.exportJSON)
	mux.HandleFunc("GET /export.csv", s.exportCSV)
	mux.HandleFunc("GET /observations/{id}/export.md", s.exportObservationMD)
	mux.HandleFunc("GET /diff", s.diff)
	mux.HandleFunc("GET /export-vault", s.exportVaultPage)
	mux.HandleFunc("POST /export-vault", s.exportVaultSubmit)
	mux.HandleFunc("GET /sync", s.syncPage)
	mux.HandleFunc("GET /sync/pull-users", s.syncPullUsers)
	mux.HandleFunc("POST /sync/push", s.syncPush)
	mux.HandleFunc("POST /sync/pull", s.syncPull)
	mux.HandleFunc("POST /sync/enroll", s.syncEnroll)
	mux.HandleFunc("POST /sync/resync", s.syncResync)
	mux.HandleFunc("POST /sync/pull-selective", s.syncPullSelective)
	mux.HandleFunc("GET /share", s.sharePage)
	mux.HandleFunc("POST /share/setup", s.shareSetup)
	mux.HandleFunc("POST /share/disconnect", s.shareDisconnect)
	mux.HandleFunc("POST /share/assign", s.shareAssign)
	mux.HandleFunc("POST /share/push", s.sharePush)
	mux.HandleFunc("POST /share/publish-anyway", s.sharePublishAnyway)
	mux.HandleFunc("POST /share/pull", s.sharePull)
	mux.HandleFunc("POST /share/pull-selective", s.sharePullSelective)
	mux.HandleFunc("POST /sync/config", s.syncConfig)
	mux.HandleFunc("POST /sync/unlink", s.syncUnlink)

	// Panics at boot rather than on the first request for a stylesheet. The
	// error only happens if the embed layout changes, and http.FS(nil) would
	// otherwise blow up mid-request as a page with no CSS and a stack trace on
	// stderr — a failure that looks like anything but a build mistake.
	sub, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		panic("embedded static/ missing: " + err.Error())
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServer(http.FS(sub)))))
	return sameOrigin(mux)
}

// cacheStatic lets the browser keep the CSS and JS between page loads.
//
// Files in an embed.FS have a zero modification time, so ServeContent emits no
// Last-Modified and FileServer generates no ETag — meaning no validator, no
// 304, and the whole stylesheet plus every script re-downloaded on each
// navigation. The assets are compiled into the binary, so they change exactly
// when the binary does: the build commit is an precise validator, and free.
//
// Falls back to no-store when the commit is unknown (a `go run` build), which
// is the right default while iterating.
func cacheStatic(next http.Handler) http.Handler {
	etag := `"` + version.Commit + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if version.Commit == "" || version.Commit == "unknown" {
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "max-age=300")
		if match := r.Header.Get("If-None-Match"); match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects state-changing requests that did not come from this UI.
//
// Listening on loopback is not access control. The port is fixed and known, so
// any page in the user's browser can post a plain <form> to 127.0.0.1:7438 and
// the browser will send it — no script, no CORS preflight, nothing to opt into.
// What that reaches here is not a toy: bulk delete, a vault export that writes
// files, and /share/setup, which hands a URL to `git clone`.
//
// The check is Fetch Metadata, not a token. Every browser since ~2020 sends
// Sec-Fetch-Site, it cannot be forged by page script, and it needs no session,
// no hidden field and no state on our side — which matters for a UI whose
// forms are also driven by htmx.
//
// Requests with neither header are let through: that is curl and scripts, and
// CSRF is a browser-only problem. An attacker who can already run code on this
// machine does not need a forged form.
//
// GET is deliberately untouched. Navigating here by typing the address or
// following a bookmark arrives as Sec-Fetch-Site: none, and blocking that
// would break the app. The one GET that mutates anything is /set-project,
// which flips the active project — annoying to have flipped, not damaging.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		switch site := r.Header.Get("Sec-Fetch-Site"); site {
		case "same-origin":
			next.ServeHTTP(w, r)
			return
		case "":
			// Older browser, or not a browser at all. Fall through to Origin.
		default: // cross-site, same-site, none
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}

		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r) // no browser context at all
			return
		}
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Projects comparison ───────────────────────────────────────────────────

type projectRow struct {
	Name           string
	Observations   int
	Sessions       int
	TopicKeys      int
	Pending        int
	ConflictsTotal int
	LastActivity   string
	// LatestSession is the most recently active session of this project, so a
	// project with a single one can be opened directly instead of through a
	// list with one row in it. Most projects are exactly that: agents keep
	// saving into one manual-save-<project> session.
	LatestSession string
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stats, err := s.C.Stats(ctx)
	if err != nil {
		s.renderError(w, r, "Failed to load stats", err)
		return
	}

	const obsScan = 500
	const sessScan = 100

	// One row is three independent HTTP calls to engram, and the rows do not
	// depend on each other, so the page used to cost 3×N round trips in strict
	// sequence — thirty of them for ten projects, all waiting on each other for
	// no reason. Fan out instead, with a small cap: engram is a single local
	// process and burying it under every project at once helps nobody.
	rows := make([]projectRow, len(stats.Projects))
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i, p := range stats.Projects {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i] = s.projectRowFor(ctx, p, obsScan, sessScan)
		}(i, p)
	}
	wg.Wait()

	sort.Slice(rows, func(i, j int) bool { return rows[i].LastActivity > rows[j].LastActivity })

	data := s.baseData(r, stats)
	data["Title"] = "Projects"
	data["Rows"] = rows
	s.render(w, "projects.html.tmpl", data)
}

// projectRowFor gathers one project's row. Each goroutine writes its own slot
// in the caller's slice, so no lock is needed.
func (s *Server) projectRowFor(ctx context.Context, p string, obsScan, sessScan int) projectRow {
	row := projectRow{Name: p}

	if obs, err := s.C.RecentObservations(ctx, p, "", obsScan, 0); err == nil {
		row.Observations = len(obs)
		topics := map[string]bool{}
		for _, o := range obs {
			if o.TopicKey != "" {
				topics[o.TopicKey] = true
			}
			if o.CreatedAt > row.LastActivity {
				row.LastActivity = o.CreatedAt
			}
		}
		row.TopicKeys = len(topics)
	}

	// Scoped by engram rather than fetched whole and filtered here, which
	// also fixes the count: the window is now this project's last 100
	// sessions instead of whatever survived a global cut across all of them.
	if sessions, err := s.C.RecentSessions(ctx, p, sessScan, 0); err == nil {
		row.Sessions = len(sessions)
		if len(sessions) > 0 {
			row.LatestSession = sessions[0].ID // engram orders by last activity
		}
		for _, sess := range sessions {
			if sess.StartedAt > row.LastActivity {
				row.LastActivity = sess.StartedAt
			}
		}
	}

	if cs, err := s.C.ConflictStats(ctx, p); err == nil && cs != nil {
		row.Pending = cs.ByJudgmentStatus["pending"]
		for _, v := range cs.ByJudgmentStatus {
			row.ConflictsTotal += v
		}
	}

	return row
}

// ── Project switcher ──────────────────────────────────────────────────────

func (s *Server) setProject(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("project")
	cookie := &http.Cookie{
		Name:     projectCookie,
		Value:    p,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 365,
	}
	if p == "" {
		cookie.MaxAge = -1
	}
	http.SetCookie(w, cookie)
	// Destination precedence: explicit ?to= (the Projects "Switch" button sends
	// to=/ so the new project's dashboard loads and the change is obvious), else
	// the Referer (the sidebar dropdown stays on the current screen, re-filtered).
	dest := localRedirect(r.URL.Query().Get("to"), r.Host)
	if dest == "" {
		dest = localRedirect(r.Header.Get("Referer"), r.Host)
	}
	if dest == "" {
		dest = "/"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// localRedirect reduces a redirect target to a path inside this app, returning
// "" for anything else so the caller falls back to a known-good page.
//
// Both sources are attacker-controlled — ?to= is a query parameter and Referer
// is a request header — and an unchecked redirect turns this UI into a
// laundering hop for a phishing link: the victim sees an address on their own
// machine and lands somewhere else entirely.
//
// Absolute URLs are accepted only when the host is ours, because Referer
// always arrives absolute and the sidebar switcher relies on it to stay on the
// current screen. "//evil.example" is rejected despite the leading slash: it
// is protocol-relative and leaves the site. That is the case people miss.
func localRedirect(dest, host string) string {
	if dest == "" {
		return ""
	}
	if u, err := url.Parse(dest); err == nil && u.Host != "" {
		if u.Host != host {
			return ""
		}
		dest = u.RequestURI()
	}
	if !strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "//") || strings.HasPrefix(dest, `/\`) {
		return ""
	}
	return dest
}

func currentTheme(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil && (c.Value == "light" || c.Value == "dark") {
		return c.Value
	}
	return "dark"
}

func currentProject(r *http.Request) string {
	if v := r.URL.Query().Get("project"); v != "" {
		return v
	}
	if c, err := r.Cookie(projectCookie); err == nil {
		return c.Value
	}
	return ""
}

// baseData collects globals injected into every screen render.
// Pre-fetched stats may be passed to avoid a duplicate /stats call.
// stats may be nil if engram is briefly unavailable; templates must guard.
func (s *Server) baseData(r *http.Request, stats *client.Stats) map[string]any {
	if stats == nil {
		stats, _ = s.C.Stats(r.Context())
	}
	data := map[string]any{
		"CurrentProject": currentProject(r),
		"Path":           r.URL.Path,
		"Theme":          currentTheme(r),
	}
	if stats != nil {
		data["Projects"] = stats.Projects
	} else {
		data["Projects"] = []string{}
	}
	return data
}

// ── Cloud provenance (author index) ───────────────────────────────────────

// authorsForProject returns a sync_id→contributor map for the project, used to
// badge and filter observations by who published them. Best-effort and cached
// (authorsTTL): any failure yields an empty map so screens render without
// badges. Pulls and pushes invalidate the entry explicitly.
//
// The shared repo is consulted first because it is free — the chunks are files
// in the local clone — while the cloud index costs one HTTP GET per chunk, which
// is exactly why this cache exists. Preferring it also keeps the answer current
// for teams that moved to git: a cloud server that stopped receiving pushes
// would otherwise keep serving an index missing everything published since.
// Projects only ever live on one transport (mixing them duplicates chunk ids),
// so first-hit-wins needs no merge.
func (s *Server) authorsForProject(ctx context.Context, project string) map[string]string {
	if project == "" {
		return nil
	}

	s.authorsMu.Lock()
	if e, ok := s.authorsCache[project]; ok && time.Since(e.fetched) < authorsTTL {
		idx := e.index
		s.authorsMu.Unlock()
		return idx
	}
	s.authorsMu.Unlock()

	idx := s.shareAuthors(ctx, project)
	if idx == nil {
		idx = s.cloudAuthors(ctx, project)
	}
	if idx == nil {
		return nil // leave screens badge-less; don't cache a failure
	}

	s.authorsMu.Lock()
	s.authorsCache[project] = authorEntry{index: idx, fetched: time.Now()}
	s.authorsMu.Unlock()
	return idx
}

// shareAuthors reads the index out of the clone the project publishes to.
// Returns nil when sharing isn't set up, the project has no repository, or it
// was never published there.
func (s *Server) shareAuthors(ctx context.Context, project string) map[string]string {
	if s.Share == nil {
		return nil
	}
	b := s.Share.For(project)
	if b == nil {
		return nil
	}
	idx, err := b.AuthorIndex(ctx, project)
	if err != nil || len(idx) == 0 {
		return nil
	}
	return idx
}

// cloudAuthors builds the index from the cloud server: one chunk download each,
// hence the timeout.
func (s *Server) cloudAuthors(ctx context.Context, project string) map[string]string {
	server := engramcli.CurrentServer()
	if s.Bridge == nil || server == "" {
		return nil
	}
	cc, err := cloudclient.New(server, s.cloudToken)
	if err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	idx, err := s.Bridge.BuildAuthorIndex(cctx, cc, project)
	if err != nil {
		return nil
	}
	return idx
}

// invalidateAuthors drops a project's cached author index so the next render
// rebuilds it. Called after push/pull, which can change cloud provenance.
func (s *Server) invalidateAuthors(project string) {
	s.authorsMu.Lock()
	delete(s.authorsCache, project)
	s.authorsMu.Unlock()

	s.pullUsersMu.Lock()
	delete(s.pullUsersCache, project)
	s.pullUsersMu.Unlock()
}

// ── Screens ───────────────────────────────────────────────────────────────

// dashboardObsScan caps the project-scoped observation count on the dashboard.
// /stats has no project filter, so we count recent obs instead.
const dashboardObsScan = 5000

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	project := currentProject(r)

	stats, err := s.C.Stats(ctx)
	if err != nil {
		s.renderError(w, r, "Failed to load stats", err)
		return
	}
	insights := s.computeInsights(ctx, project)

	// /stats reports a GLOBAL observation total (no project filter). When a
	// project is selected, scope the count to it — mirroring the Projects page.
	obsCount := stats.Observations
	capped := false
	if project != "" {
		if obs, err := s.C.RecentObservations(ctx, project, "", dashboardObsScan, 0); err == nil {
			obsCount = int64(len(obs))
			// Filling the scan window means the real number is at least this,
			// not exactly this. Presenting it bare made a project of 12000
			// observations report a flat 5000, which is worse than a rounded
			// number: it is a wrong one that looks precise.
			capped = len(obs) >= dashboardObsScan
		}
	}

	data := s.baseData(r, stats)
	data["Title"] = "Dashboard"
	data["Stats"] = stats
	data["ObsCount"] = obsCount
	data["ObsCountCapped"] = capped
	data["Insights"] = insights
	s.render(w, "dashboard.html.tmpl", data)
}

type insightTopicRev struct {
	Key           string
	Title         string
	RevisionCount int
	ObsID         int64
}

type insightSession struct {
	ID        string
	Project   string
	StartedAt string
}

type insights struct {
	TopRevisedTopics []insightTopicRev
	OrphanCount      int
	OrphanSample     []client.Observation
	NoTopicKeyCount  int
	NoTopicKeySample []client.Observation
	PendingTotal     int
	ConflictsByKind  map[string]int
	StaleSessions    []insightSession
	WindowScanned    int

	HealthScore   int
	HealthFactors []healthFactor
}

type healthFactor struct {
	Label  string
	Score  int // 0..100
	Weight int // weight in aggregate
	Detail string
}

// computeInsights derives dashboard cards from a bounded scan of recent obs +
// conflict stats. Tolerates partial failures — every block is best-effort.
func (s *Server) computeInsights(ctx context.Context, project string) insights {
	const window = 500
	out := insights{ConflictsByKind: map[string]int{}}

	if obs, err := s.C.RecentObservations(ctx, project, "", window, 0); err == nil {
		out.WindowScanned = len(obs)
		var revs []insightTopicRev
		seenTopic := map[string]bool{}
		for _, o := range obs {
			if o.Project == "" {
				out.OrphanCount++
				if len(out.OrphanSample) < 5 {
					out.OrphanSample = append(out.OrphanSample, o)
				}
			}
			if o.TopicKey == "" {
				out.NoTopicKeyCount++
				if len(out.NoTopicKeySample) < 5 {
					out.NoTopicKeySample = append(out.NoTopicKeySample, o)
				}
			}
			if o.TopicKey != "" && o.RevisionCount > 1 && !seenTopic[o.TopicKey] {
				seenTopic[o.TopicKey] = true
				revs = append(revs, insightTopicRev{
					Key:           o.TopicKey,
					Title:         o.Title,
					RevisionCount: o.RevisionCount,
					ObsID:         o.ID,
				})
			}
		}
		sort.Slice(revs, func(i, j int) bool { return revs[i].RevisionCount > revs[j].RevisionCount })
		if len(revs) > 5 {
			revs = revs[:5]
		}
		out.TopRevisedTopics = revs
	}

	if stats, err := s.C.ConflictStats(ctx, project); err == nil && stats != nil {
		out.PendingTotal = stats.ByJudgmentStatus["pending"]
		for k, v := range stats.ByRelation {
			if k == "pending" || k == "not_conflict" {
				continue
			}
			out.ConflictsByKind[k] = v
		}
	}

	// One call, scoped by engram. This used to fetch the same 50 sessions twice
	// — once to count, once to loop — and filter by project in Go, which also
	// left the summary score comparing a project's stale sessions against every
	// project's total. Counting stale separately from the 5 shown keeps the
	// score honest when there are more than five.
	scanned, stale := 0, 0
	if sessions, err := s.C.RecentSessions(ctx, project, sessionInsightScan, 0); err == nil {
		scanned = len(sessions)
		for _, sess := range sessions {
			if sess.Summary != "" || sess.EndedAt == nil {
				continue
			}
			stale++
			if len(out.StaleSessions) < 5 {
				out.StaleSessions = append(out.StaleSessions, insightSession{
					ID:        sess.ID,
					Project:   sess.Project,
					StartedAt: sess.StartedAt,
				})
			}
		}
	}

	out.HealthScore, out.HealthFactors = computeHealth(out, scanned, stale)
	return out
}

// sessionInsightScan bounds the session window the dashboard reasons about.
const sessionInsightScan = 50

// computeHealth returns aggregate score 0..100 + 4 factor breakdown.
// Weights: topic-key coverage 30, project assignment 20, judged ratio 25, summary coverage 25.
func computeHealth(in insights, sessionsScanned, staleSessions int) (int, []healthFactor) {
	pct := func(num, total int) int {
		if total <= 0 {
			return 100
		}
		v := 100 * num / total
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		return v
	}

	topicCoverage := pct(in.WindowScanned-in.NoTopicKeyCount, in.WindowScanned)
	projectCoverage := pct(in.WindowScanned-in.OrphanCount, in.WindowScanned)

	judgedScore := 100
	if in.PendingTotal > 0 {
		drop := in.PendingTotal * 2
		if drop > 100 {
			drop = 100
		}
		judgedScore = 100 - drop
	}

	summaryScore := pct(sessionsScanned-staleSessions, sessionsScanned)

	factors := []healthFactor{
		{Label: "Topic-key coverage", Score: topicCoverage, Weight: 30,
			Detail: fmt.Sprintf("%d/%d obs have a topic_key", in.WindowScanned-in.NoTopicKeyCount, in.WindowScanned)},
		{Label: "Project coverage", Score: projectCoverage, Weight: 20,
			Detail: fmt.Sprintf("%d/%d obs assigned to a project", in.WindowScanned-in.OrphanCount, in.WindowScanned)},
		{Label: "Judged ratio", Score: judgedScore, Weight: 25,
			Detail: fmt.Sprintf("%d pending conflicts", in.PendingTotal)},
		{Label: "Summary coverage", Score: summaryScore, Weight: 25,
			Detail: fmt.Sprintf("%d/%d ended sessions have a summary", sessionsScanned-staleSessions, sessionsScanned)},
	}

	totalW, weighted := 0, 0
	for _, f := range factors {
		totalW += f.Weight
		weighted += f.Score * f.Weight
	}
	score := 0
	if totalW > 0 {
		score = weighted / totalW
	}
	return score, factors
}

// searchRedirect keeps /search working: bookmarks and anything the team pasted
// into a chat still resolve, now onto the one screen that owns observations.
func (s *Server) searchRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/observations"
	if raw := r.URL.RawQuery; raw != "" {
		target += "?" + raw
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// authorScanLimit bounds the "filter by contributor" pass. engram has no
// author filter — authorship isn't in its database at all, only on sync chunks
// — so the filtering happens here, over a wide page of recent observations.
// The window is finite and the UI says so; the same trick and the same order of
// magnitude as dashboardObsScan.
const authorScanLimit = 5000

// observations is the one screen for looking at observations: the most recent
// by default, narrowed when any filter is used.
//
// Search and "recent" were two routes with two templates that were the same
// template — identical list markup, identical bulk bar, identical badges —
// differing only in where the rows came from and which filters they offered. A
// full-text query is a WHERE clause on the same question, so it is the same
// page, and merging them means the bulk actions, the contributor filter and the
// author badges now work on search results too.
func (s *Server) observations(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	scope := r.URL.Query().Get("scope")
	typ := r.URL.Query().Get("type")
	createdBy := strings.TrimSpace(r.URL.Query().Get("created_by"))
	// One day, YYYY-MM-DD. The heatmap has always linked here with it; nothing
	// ever read it, so clicking a cell silently showed the unfiltered list.
	since := strings.TrimSpace(r.URL.Query().Get("since"))
	if !validDay(since) {
		since = ""
	}
	limit := atoiOr(r.URL.Query().Get("limit"), 25, maxPageLimit)
	offset := atoiOrZero(r.URL.Query().Get("offset"))
	project := currentProject(r)
	authors := s.authorsForProject(r.Context(), project)

	// What engram can filter depends entirely on which endpoint we land on.
	// /search takes type and scope; /observations/recent takes scope but not
	// type, and neither of them has ever heard of an author. Everything engram
	// cannot do we do here, over a wide window — which is why the page reports
	// how far back it looked and warns when it hits the end of it.
	narrowHere := createdBy != "" || since != "" || (q == "" && typ != "")
	fetch, from := limit, offset
	if narrowHere {
		// Scan wide from the top and page the filtered slice ourselves.
		fetch, from = authorScanLimit, 0
	}

	var obs []client.Observation
	if q != "" {
		res, err := s.C.Search(r.Context(), client.SearchQuery{
			Query:   q,
			Project: project,
			Scope:   scope,
			Type:    typ,
			Limit:   fetch,
			Offset:  from,
		})
		if err != nil {
			s.renderError(w, r, "Search failed", err)
			return
		}
		obs = res.Results
	} else {
		recent, err := s.C.RecentObservations(r.Context(), project, scope, fetch, from)
		if err != nil {
			s.renderError(w, r, "Failed to load observations", err)
			return
		}
		obs = recent
	}

	scanned, matched := len(obs), len(obs)
	if narrowHere {
		obs = narrow(obs, scope, typ, createdBy, since, authors)
		matched = len(obs)
		obs = page(obs, offset, limit)
	}

	data := s.baseData(r, nil)
	data["Title"] = "Observations"
	data["Observations"] = obs
	data["Query"] = q
	data["Scope"] = scope
	data["Type"] = typ
	data["Scopes"] = canonicalScopes
	data["Types"] = canonicalTypes
	data["CreatedBy"] = createdBy
	data["Since"] = since
	data["Contributors"] = distinctAuthors(authors)
	data["Authors"] = authors
	data["Limit"] = limit
	data["Offset"] = offset
	data["BasePath"] = "/observations?q=" + escape(q) +
		"&scope=" + escape(scope) + "&type=" + escape(typ) +
		"&created_by=" + escape(createdBy) + "&since=" + escape(since)
	data["HasNext"] = len(obs) == limit
	data["ShowPagination"] = offset > 0 || len(obs) == limit
	if narrowHere {
		data["MatchedCount"] = matched
		data["ScannedCount"] = scanned
		data["ScanCapped"] = scanned >= authorScanLimit
	}
	s.render(w, "observations.html.tmpl", data)
}

// narrow applies the filters engram could not. Empty values mean "no opinion",
// so this is a no-op when nothing was asked for, and safe to run on results
// engram already filtered.
//
// Type is compared case-insensitively because the value comes from a list of
// canonical types while the stored one is whatever was saved.
//
// Author is the odd one: engram stores none, so it is looked up in the index
// built from the sync commits. An observation with no sync_id has never been
// published by anyone, so it belongs to nobody and matches no contributor.
func narrow(obs []client.Observation, scope, typ, createdBy, since string, authors map[string]string) []client.Observation {
	out := make([]client.Observation, 0, len(obs))
	for _, o := range obs {
		switch {
		case scope != "" && !strings.EqualFold(o.Scope, scope):
		case typ != "" && !strings.EqualFold(o.Type, typ):
		case createdBy != "" && (o.SyncID == "" || authors[o.SyncID] != createdBy):
		// engram stores created_at as "YYYY-MM-DD HH:MM:SS", so one day is a
		// string prefix — the same trick activityData already uses to bucket
		// the heatmap this filter is reached from.
		case since != "" && !strings.HasPrefix(o.CreatedAt, since):
		default:
			out = append(out, o)
		}
	}
	return out
}

// validDay accepts YYYY-MM-DD and nothing else. The value becomes a prefix
// match, so a loose one would quietly match more than a day.
func validDay(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// page slices one page out of a list, tolerating an offset past the end.
func page[T any](items []T, offset, limit int) []T {
	if offset >= len(items) {
		return nil
	}
	items = items[offset:]
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

// distinctAuthors lists the contributors present in an index, sorted, for the
// filter dropdown.
func distinctAuthors(authors map[string]string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, a := range authors {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) observationDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, "Invalid observation id", err)
		return
	}
	obs, err := s.C.GetObservation(r.Context(), id)
	if err != nil {
		s.renderError(w, r, fmt.Sprintf("Failed to load observation #%d", id), err)
		return
	}
	data := s.baseData(r, nil)
	data["Title"] = fmt.Sprintf("Observation #%d", id)
	data["Obs"] = obs
	if obs.SyncID != "" {
		if authors := s.authorsForProject(r.Context(), obs.Project); authors != nil {
			data["CreatedBy"] = authors[obs.SyncID]
		}
	}
	s.render(w, "observation_detail.html.tmpl", data)
}

func (s *Server) observationEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, "Invalid observation id", err)
		return
	}
	obs, err := s.C.GetObservation(r.Context(), id)
	if err != nil {
		s.renderError(w, r, "Failed to load observation", err)
		return
	}
	data := s.baseData(r, nil)
	data["Title"] = fmt.Sprintf("Edit #%d", id)
	data["Obs"] = obs
	data["Types"] = canonicalTypes
	data["Scopes"] = canonicalScopes
	s.render(w, "observation_edit.html.tmpl", data)
}

func (s *Server) observationEditSubmit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, "Invalid observation id", err)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	req := UpdateRequestFromForm(r)
	if _, err := s.C.UpdateObservation(r.Context(), id, req); err != nil {
		s.renderError(w, r, "Update failed", err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/observations/%d", id), http.StatusSeeOther)
}

func (s *Server) observationDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, "Invalid observation id", err)
		return
	}
	hard := r.URL.Query().Get("hard") == "true"
	if err := s.C.DeleteObservation(r.Context(), id, hard); err != nil {
		s.renderError(w, r, "Delete failed", err)
		return
	}
	http.Redirect(w, r, "/observations", http.StatusSeeOther)
}

// UpdateRequestFromForm only sets fields the user actually submitted.
// Empty form values are preserved as nil so PATCH does not zero them out.
func UpdateRequestFromForm(r *http.Request) client.UpdateObservationRequest {
	var req client.UpdateObservationRequest
	maybe := func(name string) *string {
		if !r.Form.Has(name) {
			return nil
		}
		v := strings.TrimSpace(r.FormValue(name))
		return &v
	}
	req.Title = maybe("title")
	req.Content = maybe("content")
	req.Type = maybe("type")
	req.Scope = maybe("scope")
	req.TopicKey = maybe("topic_key")
	req.Project = maybe("project")
	return req
}

// observationsBulk runs an action on a list of comma-separated obs ids.
// Form: ids=1,2,3 action=delete|patch_scope|patch_type|patch_topic_prefix value=...
func (s *Server) observationsBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	rawIDs := r.Form["ids"]
	action := r.FormValue("action")
	value := strings.TrimSpace(r.FormValue("value"))

	var ids []int64
	for _, raw := range rawIDs {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			n, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				continue
			}
			ids = append(ids, n)
		}
	}
	if len(ids) == 0 {
		s.renderError(w, r, "No ids selected", errors.New("empty"))
		return
	}

	ctx := r.Context()
	var failed []int64
	for _, id := range ids {
		var err error
		switch action {
		case "delete":
			err = s.C.DeleteObservation(ctx, id, false)
		case "patch_scope":
			err = patchObs(ctx, s.C, id, client.UpdateObservationRequest{Scope: &value})
		case "patch_type":
			err = patchObs(ctx, s.C, id, client.UpdateObservationRequest{Type: &value})
		case "patch_topic_prefix":
			// Replace topic_key family prefix while keeping suffix.
			obs, gerr := s.C.GetObservation(ctx, id)
			if gerr != nil {
				err = gerr
				break
			}
			suffix := obs.TopicKey
			if i := strings.Index(obs.TopicKey, "/"); i >= 0 {
				suffix = obs.TopicKey[i+1:]
			}
			next := value
			if suffix != "" {
				next = value + "/" + suffix
			}
			err = patchObs(ctx, s.C, id, client.UpdateObservationRequest{TopicKey: &next})
		default:
			s.renderError(w, r, "Unknown bulk action", errors.New(action))
			return
		}
		if err != nil {
			failed = append(failed, id)
		}
	}

	dest := localRedirect(r.Header.Get("Referer"), r.Host)
	if dest == "" {
		dest = "/observations"
	}
	if len(failed) > 0 {
		s.renderError(w, r, fmt.Sprintf("Bulk %s: %d/%d failed (ids: %v)", action, len(failed), len(ids), failed), errors.New("partial failure"))
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func patchObs(ctx context.Context, c *client.Client, id int64, req client.UpdateObservationRequest) error {
	_, err := c.UpdateObservation(ctx, id, req)
	return err
}

func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, "Invalid observation id", err)
		return
	}
	before := atoiOr(r.URL.Query().Get("before"), 5, maxPageLimit)
	after := atoiOr(r.URL.Query().Get("after"), 5, maxPageLimit)
	tl, err := s.C.Timeline(r.Context(), id, before, after)
	if err != nil {
		s.renderError(w, r, fmt.Sprintf("Failed to load timeline for #%d", id), err)
		return
	}
	data := s.baseData(r, nil)
	data["Title"] = fmt.Sprintf("Timeline #%d", id)
	data["Focus"] = tl.Focus
	data["Before"] = tl.Before
	data["After"] = tl.After
	s.render(w, "timeline.html.tmpl", data)
}

func (s *Server) recentSessions(w http.ResponseWriter, r *http.Request) {
	limit := atoiOr(r.URL.Query().Get("limit"), 25, maxPageLimit)
	offset := atoiOrZero(r.URL.Query().Get("offset"))
	sessions, err := s.C.RecentSessions(r.Context(), currentProject(r), limit, offset)
	if err != nil {
		s.renderError(w, r, "Failed to load sessions", err)
		return
	}
	data := s.baseData(r, nil)
	data["Title"] = "Sessions"
	data["Sessions"] = sessions
	data["Limit"] = limit
	data["Offset"] = offset
	data["BasePath"] = "/sessions?"
	data["HasNext"] = len(sessions) == limit
	data["ShowPagination"] = offset > 0 || len(sessions) == limit
	s.render(w, "sessions.html.tmpl", data)
}

type timelineEntry struct {
	Kind      string // "obs" | "prompt"
	CreatedAt string
	Obs       *client.Observation
	Prompt    *client.Prompt
}

func (s *Server) sessionDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.renderError(w, r, "Missing session id", errors.New("empty"))
		return
	}
	ctx := r.Context()
	sess, err := s.C.GetSession(ctx, id)
	if err != nil {
		s.renderError(w, r, "Failed to load session", err)
		return
	}

	// engram has no /sessions/{id}/observations route; scan a generous window
	// of project obs + prompts and filter to this session.
	const scan = 1000
	project := sess.Project
	// Not `obs, _ :=`. A failure here used to render as an empty timeline,
	// which reads as "this session recorded nothing" — the one conclusion the
	// page must never state on a lack of data it never actually fetched.
	obs, err := s.C.RecentObservations(ctx, project, "", scan, 0)
	if err != nil {
		s.renderError(w, r, "Failed to load this session's observations", err)
		return
	}
	prompts, err := s.C.RecentPrompts(ctx, project, scan)
	if err != nil {
		s.renderError(w, r, "Failed to load this session's prompts", err)
		return
	}

	var entries []timelineEntry
	for i := range obs {
		if obs[i].SessionID == id {
			o := obs[i]
			entries = append(entries, timelineEntry{Kind: "obs", CreatedAt: o.CreatedAt, Obs: &o})
		}
	}
	for i := range prompts {
		if prompts[i].SessionID == id {
			p := prompts[i]
			entries = append(entries, timelineEntry{Kind: "prompt", CreatedAt: p.CreatedAt, Prompt: &p})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].CreatedAt < entries[j].CreatedAt })

	byType := map[string]int{}
	for _, e := range entries {
		if e.Kind == "obs" && e.Obs != nil {
			byType[e.Obs.Type]++
		} else if e.Kind == "prompt" {
			byType["(prompt)"]++
		}
	}

	data := s.baseData(r, nil)
	data["Title"] = "Session " + id
	data["Session"] = sess
	data["Entries"] = entries
	data["ByType"] = byType
	s.render(w, "session_detail.html.tmpl", data)
}

// ── Conflicts ─────────────────────────────────────────────────────────────

func (s *Server) conflicts(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}
	relationFilter := r.URL.Query().Get("relation")
	limit := atoiOr(r.URL.Query().Get("limit"), 25, maxPageLimit)
	offset := atoiOrZero(r.URL.Query().Get("offset"))
	project := currentProject(r)

	page, err := s.C.Conflicts(r.Context(), project, status, limit, offset)
	if err != nil {
		s.renderError(w, r, "Failed to load conflicts", err)
		return
	}

	// Client-side relation filter (engram /conflicts has no relation param).
	// Remember how full the page was BEFORE filtering: that is what says
	// whether engram has more, and it is the only honest basis for "next".
	// Deciding it after the filter meant one page whose rows all failed the
	// filter ended the pagination and hid everything behind it.
	fullPage := len(page.Relations) == limit
	if relationFilter != "" && page != nil {
		filtered := page.Relations[:0]
		for _, rel := range page.Relations {
			if rel.Relation == relationFilter {
				filtered = append(filtered, rel)
			}
		}
		page.Relations = filtered
	}

	stats, _ := s.C.ConflictStats(r.Context(), project)

	data := s.baseData(r, nil)
	data["Title"] = "Conflicts"
	data["Page"] = page
	data["Stats"] = stats
	data["Status"] = status
	data["RelationFilter"] = relationFilter
	data["Limit"] = limit
	data["Offset"] = offset
	data["BasePath"] = "/conflicts?status=" + escape(status) + "&relation=" + escape(relationFilter)
	data["Statuses"] = []string{"pending", "judged", "orphaned", "ignored"}
	data["RelationFilters"] = canonicalRelations
	data["AgentCLI"] = agentCLI()
	data["Scan"] = s.scanState()
	data["HasNext"] = fullPage
	data["ShowPagination"] = offset > 0 || fullPage
	s.render(w, "conflicts.html.tmpl", data)
}

// scanConflicts starts POST /conflicts/scan in the background and redirects.
// Body fields: apply=on, semantic=on, max_semantic=int, concurrency=int.
//
// Always uses the current cookie-selected project (engram requires project).
// A scan already in flight wins: the redirect lands on the card that says so.
func (s *Server) scanConflicts(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	project := currentProject(r)
	if project == "" {
		s.renderError(w, r, "Pick a project first", errors.New("project required for scan"))
		return
	}

	semantic := r.FormValue("semantic") == "on"
	if semantic && agentCLI() == "" {
		// engram answers this with a 500 whose body is a Go error string. It is
		// a configuration question, not a failure, so it gets answered here.
		s.renderError(w, r, "The LLM judge is not configured",
			errors.New("set ENGRAM_AGENT_CLI to \"claude\" or \"opencode\" and restart engram-ui, "+
				"or clear \"Let the LLM judge them\" to scan without it — the pairs it finds are then left pending for you"))
		return
	}

	req := client.ScanRequest{
		Project:     project,
		Apply:       r.FormValue("apply") == "on",
		Semantic:    semantic,
		MaxSemantic: atoiOrZero(r.FormValue("max_semantic")),
		MaxInsert:   atoiOrZero(r.FormValue("max_insert")),
		Concurrency: atoiOrZero(r.FormValue("concurrency")),
	}
	if s.claimScan(project) {
		go func() {
			// context.Background(), not r.Context(): the redirect below ends
			// the request, which would cancel our wait for the result the
			// instant it started. engram would not notice — ScanProject takes
			// no context — and would scan to the end with nobody listening.
			res, err := s.C.ScanConflicts(context.Background(), req)
			s.finishScan(res, err)
		}()
	}
	http.Redirect(w, r, "/conflicts", http.StatusSeeOther)
}

// scanStatus is what the running-scan card polls. It answers from memory and
// never touches engram — which is the whole point: while a scan runs, engram
// serves everything through one database connection, and a poll that queried
// it would be both slow and part of the problem.
//
// A finished scan answers HX-Refresh instead of a fragment: the counters, the
// pending list and the result all changed, and only one of them is in here.
func (s *Server) scanStatus(w http.ResponseWriter, r *http.Request) {
	job := s.scanState()
	if job == nil || job.Done {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.renderFragment(w, "conflicts.html.tmpl", "scan-status", map[string]any{
		"Scan":           job,
		"CurrentProject": currentProject(r),
		"AgentCLI":       agentCLI(),
	})
}

func (s *Server) conflictDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, "Invalid conflict id", err)
		return
	}
	det, err := s.C.Conflict(r.Context(), id)
	if err != nil {
		s.renderError(w, r, "Failed to load conflict", err)
		return
	}
	data := s.baseData(r, nil)
	data["Title"] = fmt.Sprintf("Conflict #%d", id)
	data["Conflict"] = det
	data["Relations"] = canonicalRelations
	s.render(w, "conflict_detail.html.tmpl", data)
}

func (s *Server) judgeConflict(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	syncID := r.FormValue("sync_id")
	relation := r.FormValue("relation")
	reason := r.FormValue("reason")
	// Discarding the parse error turned any unparseable value into 0, which the
	// line below then promoted to 1.0 — so junk in the field was recorded as
	// MAXIMUM confidence in a verdict about someone's memories. Refuse instead,
	// and clamp the rest: engram rejects anything outside [0,1] with a 400, and
	// finding that out after the redirect is a worse way to learn it.
	confidence := 1.0
	if raw := strings.TrimSpace(r.FormValue("confidence")); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			s.renderError(w, r, "Confidence must be a number between 0 and 1", err)
			return
		}
		if parsed < 0 || parsed > 1 {
			s.renderError(w, r, fmt.Sprintf("Confidence must be between 0 and 1, got %v", parsed),
				errors.New("out of range"))
			return
		}
		confidence = parsed
	}
	if syncID == "" || relation == "" {
		s.renderError(w, r, "Missing sync_id or relation", errors.New("required fields missing"))
		return
	}
	if err := s.C.JudgeConflict(r.Context(), syncID, relation, reason, confidence); err != nil {
		s.renderError(w, r, "Judge failed", err)
		return
	}
	http.Redirect(w, r, "/conflicts", http.StatusSeeOther)
}

// ── Prompts ───────────────────────────────────────────────────────────────

func (s *Server) recentPrompts(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := atoiOr(r.URL.Query().Get("limit"), 25, maxPageLimit)
	project := currentProject(r)

	var prompts []client.Prompt
	var err error
	if q != "" {
		prompts, err = s.C.SearchPrompts(r.Context(), q, project, limit)
	} else {
		prompts, err = s.C.RecentPrompts(r.Context(), project, limit)
	}
	if err != nil {
		s.renderError(w, r, "Failed to load prompts", err)
		return
	}
	data := s.baseData(r, nil)
	data["Title"] = "Prompts"
	data["Query"] = q
	data["Prompts"] = prompts
	data["Limit"] = limit
	data["ShowPagination"] = false
	s.render(w, "prompts.html.tmpl", data)
}

// ── Topics (topic-key explorer) ───────────────────────────────────────────

type topicGroup struct {
	Family string
	Keys   []topicKey
}

type topicKey struct {
	Key           string
	Title         string
	Project       string
	Type          string
	RevisionCount int
	UpdatedAt     string
	LatestID      int64
}

// untaggedShown caps the "no topic-key yet" list. The count is exact for the
// scanned window; showing every one of them would bury the keys themselves.
const untaggedShown = 25

// topics groups observations by topic_key. engram has no native endpoint,
// so we scan recent observations across the current project. Limited window.
func (s *Server) topics(w http.ResponseWriter, r *http.Request) {
	project := currentProject(r)
	scan := atoiOr(r.URL.Query().Get("scan"), 500, maxScanLimit)

	obs, err := s.C.RecentObservations(r.Context(), project, "", scan, 0)
	if err != nil {
		s.renderError(w, r, "Failed to scan observations", err)
		return
	}

	// dedup by TopicKey, keep latest (RecentObservations is created_at desc).
	// The ones WITHOUT a key are collected in the same pass: they are the whole
	// point of the screen's other half — "topic-key coverage: 4/24" is only
	// actionable if you can see the other twenty, and they exist nowhere else
	// in the UI beyond a five-item sample on the dashboard.
	seen := map[string]*topicKey{}
	var untagged []client.Observation
	for _, o := range obs {
		if o.TopicKey == "" {
			untagged = append(untagged, o)
			continue
		}
		if _, ok := seen[o.TopicKey]; ok {
			continue
		}
		seen[o.TopicKey] = &topicKey{
			Key:           o.TopicKey,
			Title:         o.Title,
			Project:       o.Project,
			Type:          o.Type,
			RevisionCount: o.RevisionCount,
			UpdatedAt:     coalesce(o.UpdatedAt, o.CreatedAt),
			LatestID:      o.ID,
		}
	}

	groupsMap := map[string][]topicKey{}
	for _, k := range seen {
		family := "(other)"
		if i := strings.Index(k.Key, "/"); i > 0 {
			family = k.Key[:i]
		}
		groupsMap[family] = append(groupsMap[family], *k)
	}

	families := make([]string, 0, len(groupsMap))
	for f := range groupsMap {
		families = append(families, f)
	}
	sortStrings(families)
	groups := make([]topicGroup, 0, len(families))
	for _, f := range families {
		keys := groupsMap[f]
		sortTopicKeysByKey(keys)
		groups = append(groups, topicGroup{Family: f, Keys: keys})
	}

	data := s.baseData(r, nil)
	data["Title"] = "Topic-keys"
	data["Groups"] = groups
	data["Scanned"] = len(obs)
	data["Scan"] = scan
	data["UntaggedCount"] = len(untagged)
	data["Untagged"] = page(untagged, 0, untaggedShown)
	s.render(w, "topics.html.tmpl", data)
}

// topicDetail lists every observation sharing a topic_key (revisions chronological).
// engram exposes no /topics/{key}; we use /search with the topic_key as q.
func (s *Server) topicDetail(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		s.renderError(w, r, "Missing topic key", errors.New("empty"))
		return
	}
	project := currentProject(r)

	res, err := s.C.Search(r.Context(), client.SearchQuery{
		Query:   key,
		Project: project,
		Limit:   100,
	})
	if err != nil {
		s.renderError(w, r, "Failed to load topic", err)
		return
	}

	revs := make([]client.Observation, 0, len(res.Results))
	for _, o := range res.Results {
		if o.TopicKey == key {
			revs = append(revs, o)
		}
	}

	data := s.baseData(r, nil)
	data["Title"] = "Topic · " + key
	data["Key"] = key
	data["Revisions"] = revs
	s.render(w, "topic_detail.html.tmpl", data)
}

// ── Manual save ───────────────────────────────────────────────────────────

func (s *Server) savePage(w http.ResponseWriter, r *http.Request) {
	data := s.baseData(r, nil)
	data["Title"] = "Save observation"
	data["Types"] = canonicalTypes
	data["Scopes"] = canonicalScopes
	data["DefaultProject"] = currentProject(r)
	s.render(w, "save.html.tmpl", data)
}

func (s *Server) saveSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	content := strings.TrimSpace(r.FormValue("content"))
	if title == "" || content == "" {
		s.renderError(w, r, "title and content are required", errors.New("missing fields"))
		return
	}

	project := strings.TrimSpace(r.FormValue("project"))
	if project == "" {
		project = currentProject(r)
	}
	if project == "" {
		s.renderError(w, r, "project is required", errors.New("no project"))
		return
	}

	sessionID := "manual-save-" + project
	if err := s.C.CreateSession(r.Context(), sessionID, project, ""); err != nil {
		s.renderError(w, r, "Failed to ensure session", err)
		return
	}

	id, err := s.C.AddObservation(r.Context(), client.AddObservationRequest{
		SessionID: sessionID,
		Type:      strings.TrimSpace(r.FormValue("type")),
		Title:     title,
		Content:   content,
		Project:   project,
		Scope:     strings.TrimSpace(r.FormValue("scope")),
		TopicKey:  strings.TrimSpace(r.FormValue("topic_key")),
	})
	if err != nil {
		s.renderError(w, r, "Save failed", err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/observations/%d", id), http.StatusSeeOther)
}

// ── Exports ───────────────────────────────────────────────────────────────

func (s *Server) exportObservationMD(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, "Invalid observation id", err)
		return
	}
	o, err := s.C.GetObservation(r.Context(), id)
	if err != nil {
		s.renderError(w, r, "Failed to load observation", err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="observation-%d.md"`, id))
	fmt.Fprintf(w, "---\nid: %d\ntype: %s\ntitle: %q\nproject: %s\nscope: %s\ntopic_key: %s\ncreated_at: %s\n",
		o.ID, o.Type, o.Title, o.Project, o.Scope, o.TopicKey, o.CreatedAt)
	if o.UpdatedAt != "" {
		fmt.Fprintf(w, "updated_at: %s\n", o.UpdatedAt)
	}
	fmt.Fprintf(w, "---\n\n# %s\n\n%s\n", o.Title, o.Content)
}

// backupSnapshot proxies engram /export with auth, streams the JSON to the
// browser as engram-backup-<project>-<timestamp>.json. localStorage on the
// client tracks snapshot history.
func (s *Server) backupSnapshot(w http.ResponseWriter, r *http.Request) {
	// ?all=1 is every project. An explicit ?project= wins otherwise, and a bare
	// /backup falls back to whatever you are looking at — "everything" needs to
	// be asked for, since it cannot be told apart from "no opinion".
	project := r.URL.Query().Get("project")
	if r.URL.Query().Get("all") == "1" {
		project = ""
	} else if project == "" {
		project = currentProject(r)
	}
	body, _, err := s.C.Export(r.Context(), project)
	if err != nil {
		s.renderError(w, r, "Backup failed", err)
		return
	}
	defer body.Close()
	ts := time.Now().UTC().Format("20060102T150405Z")
	tag := project
	if tag == "" {
		tag = "all"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="engram-backup-%s-%s.json"`, slug(tag), ts))
	if _, err := io.Copy(w, body); err != nil {
		// can't change status after WriteHeader; client sees truncated body
		_ = err
	}
}

// exportJSON streams the current search/recent observations result as JSON.
func (s *Server) exportJSON(w http.ResponseWriter, r *http.Request) {
	obs, err := s.exportObservations(r)
	if err != nil {
		s.renderError(w, r, "Export failed", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="engram-export.json"`)
	enc := jsonEncoder(w)
	if err := enc.Encode(obs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) exportCSV(w http.ResponseWriter, r *http.Request) {
	obs, err := s.exportObservations(r)
	if err != nil {
		s.renderError(w, r, "Export failed", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="engram-export.csv"`)
	cw := csvWriter(w)
	if err := cw.Write([]string{"id", "type", "title", "project", "scope", "topic_key", "created_at", "updated_at", "content"}); err != nil {
		return // the browser is gone; nothing useful left to do
	}
	for _, o := range obs {
		if err := cw.Write([]string{
			strconv.FormatInt(o.ID, 10), csvSafe(o.Type), csvSafe(o.Title), csvSafe(o.Project),
			csvSafe(o.Scope), csvSafe(o.TopicKey), o.CreatedAt, o.UpdatedAt, csvSafe(o.Content),
		}); err != nil {
			return
		}
	}
	// Flushed explicitly, not deferred: Flush is where buffered rows actually
	// reach the socket, and its error was the one worth having.
	cw.Flush()
	if err := cw.Error(); err != nil {
		// Too late for a status code — the 200 and the attachment header are
		// already sent — so the browser gets a short file. Say so in the log
		// rather than nowhere.
		log.Printf("csv export truncated: %v", err)
	}
}

// csvSafe defuses a cell that a spreadsheet would execute instead of display.
//
// Excel, LibreOffice and Sheets treat a leading =, +, -, @, tab or CR as the
// start of a formula, so an observation whose title begins with one becomes
// live content in the exported file — the classic path being a =HYPERLINK or a
// DDE call that fires on open. These observations arrive from other machines
// over sync, so the text is not ours to trust.
//
// A leading apostrophe is the conventional fix and every spreadsheet strips it
// on import, so the value still reads correctly.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// exportObservations honors current filters (?q, scope, type, project) and limit.
func (s *Server) exportObservations(r *http.Request) ([]client.Observation, error) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := atoiOr(r.URL.Query().Get("limit"), 500, maxScanLimit)
	project := currentProject(r)
	if q == "" {
		return s.C.RecentObservations(r.Context(), project, "", limit, 0)
	}
	res, err := s.C.Search(r.Context(), client.SearchQuery{
		Query:   q,
		Project: project,
		Scope:   r.URL.Query().Get("scope"),
		Type:    r.URL.Query().Get("type"),
		Limit:   limit,
	})
	if err != nil {
		return nil, err
	}
	return res.Results, nil
}

// ── Activity heatmap ──────────────────────────────────────────────────────

type activityDay struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// activityData returns per-day obs counts for the last N days. Buckets parse
// CreatedAt's date prefix ("YYYY-MM-DD"). engram lacks a dedicated activity
// endpoint; we scan a generous recent window.
func (s *Server) activityData(w http.ResponseWriter, r *http.Request) {
	project := currentProject(r)
	days := atoiOr(r.URL.Query().Get("days"), 182, maxHeatmapDays) // 26 weeks
	scan := atoiOr(r.URL.Query().Get("scan"), 2000, maxScanLimit)

	obs, err := s.C.RecentObservations(r.Context(), project, "", scan, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	counts := map[string]int{}
	for _, o := range obs {
		if len(o.CreatedAt) < 10 {
			continue
		}
		counts[o.CreatedAt[:10]]++
	}

	out := make([]activityDay, 0, days)
	now := time.Now().UTC()
	for i := days - 1; i >= 0; i-- {
		d := now.AddDate(0, 0, -i).Format("2006-01-02")
		out = append(out, activityDay{Date: d, Count: counts[d]})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = jsonEncoder(w).Encode(out)
}

// ── Search suggest (live JSON for /save) ──────────────────────────────────

func (s *Server) searchSuggest(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		_, _ = w.Write([]byte("[]"))
		return
	}
	limit := atoiOr(r.URL.Query().Get("limit"), 8, maxPageLimit)
	project := r.URL.Query().Get("project")
	if project == "" {
		project = currentProject(r)
	}
	res, err := s.C.Search(r.Context(), client.SearchQuery{
		Query:   sanitizeFTS(q),
		Project: project,
		Limit:   limit,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = jsonEncoder(w).Encode(res.Results)
}

// sanitizeFTS strips chars that break FTS5 MATCH parsing (operators, quotes).
// Keeps tokens space-separated so engram's parser can OR them.
var ftsBadChars = regexp.MustCompile(`[^\p{L}\p{N}\s_/-]`)

func sanitizeFTS(s string) string {
	s = ftsBadChars.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// ── Graph ─────────────────────────────────────────────────────────────────

func (s *Server) graphPage(w http.ResponseWriter, r *http.Request) {
	data := s.baseData(r, nil)
	data["Title"] = "Graph"
	s.render(w, "graph.html.tmpl", data)
}

type graphNode struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Type    string `json:"type"`
	Project string `json:"project,omitempty"`
	ObsID   int64  `json:"obs_id"`
}

type graphEdge struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	Relation string `json:"relation"`
	Status   string `json:"status"`
}

type graphPayload struct {
	Project  string      `json:"project"`
	Family   string      `json:"family,omitempty"`
	Families []string    `json:"families"`
	Nodes    []graphNode `json:"nodes"`
	Edges    []graphEdge `json:"edges"`
	Stats    struct {
		Observations int `json:"observations"`
		Relations    int `json:"relations"`
	} `json:"stats"`
}

// collectFamilies returns sorted unique topic-key family prefixes from a
// recent obs window.
func collectFamilies(obs []client.Observation) []string {
	set := map[string]bool{}
	for _, o := range obs {
		if o.TopicKey == "" {
			continue
		}
		i := strings.Index(o.TopicKey, "/")
		if i <= 0 {
			continue
		}
		set[o.TopicKey[:i]] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// filterGraphByFamily keeps nodes whose obs.topic_key starts "family/" plus
// any directly connected neighbor (1-hop). Edges are preserved iff both
// endpoints survive.
func filterGraphByFamily(nodes []graphNode, edges []graphEdge, family string, recent []client.Observation) ([]graphNode, []graphEdge) {
	bySync := map[string]client.Observation{}
	for _, o := range recent {
		if o.SyncID != "" {
			bySync[o.SyncID] = o
		}
	}
	prefix := family + "/"

	matched := map[string]bool{}
	for _, n := range nodes {
		o, ok := bySync[n.ID]
		if !ok {
			continue
		}
		if o.TopicKey == family || strings.HasPrefix(o.TopicKey, prefix) {
			matched[n.ID] = true
		}
	}

	keep := map[string]bool{}
	for id := range matched {
		keep[id] = true
	}
	for _, e := range edges {
		if matched[e.Source] {
			keep[e.Target] = true
		}
		if matched[e.Target] {
			keep[e.Source] = true
		}
	}

	var fnodes []graphNode
	for _, n := range nodes {
		if keep[n.ID] {
			fnodes = append(fnodes, n)
		}
	}
	var fedges []graphEdge
	for _, e := range edges {
		if keep[e.Source] && keep[e.Target] {
			fedges = append(fedges, e)
		}
	}
	return fnodes, fedges
}

// graphData returns nodes (observations) + edges (memory_relations) for the
// current project. Walks /conflicts paginated to collect all relations, then
// resolves only those obs that participate (keeps payload small for big repos).
//
// Optional ?family=X filters nodes to those with topic_key starting "X/" plus
// their immediate neighbors (so context isn't dropped).
func (s *Server) graphData(w http.ResponseWriter, r *http.Request) {
	project := currentProject(r)
	family := strings.TrimSpace(r.URL.Query().Get("family"))

	// Pull relations across all judgment statuses.
	statuses := []string{"pending", "judged", "orphaned", "ignored"}
	seenEdges := map[string]bool{}
	syncIDs := map[string]bool{}
	var edges []graphEdge
	for _, st := range statuses {
		offset := 0
		for {
			page, err := s.C.Conflicts(r.Context(), project, st, 200, offset)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, rel := range page.Relations {
				key := fmt.Sprintf("%d", rel.ID)
				if seenEdges[key] {
					continue
				}
				seenEdges[key] = true
				edges = append(edges, graphEdge{
					ID:       "e" + key,
					Source:   rel.SourceID,
					Target:   rel.TargetID,
					Relation: rel.Relation,
					Status:   rel.JudgmentStatus,
				})
				syncIDs[rel.SourceID] = true
				syncIDs[rel.TargetID] = true
			}
			if len(page.Relations) < 200 {
				break
			}
			offset += 200
		}
	}

	// Build node set from edges + recent obs (so isolated nodes appear too).
	nodes := make([]graphNode, 0, len(syncIDs))
	addNode := func(id, label, typ, proj string, obsID int64) {
		nodes = append(nodes, graphNode{ID: id, Label: label, Type: typ, Project: proj, ObsID: obsID})
	}

	// Resolve sync_ids → obs metadata. Recent endpoint is bulk; filter from there.
	recent, err := s.C.RecentObservations(r.Context(), project, "", 1000, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bySync := map[string]client.Observation{}
	for _, o := range recent {
		if o.SyncID != "" {
			bySync[o.SyncID] = o
		}
	}

	// Add edge participants first (they may be older than the recent window).
	for sid := range syncIDs {
		if o, ok := bySync[sid]; ok {
			addNode(sid, o.Title, o.Type, o.Project, o.ID)
			delete(bySync, sid)
		} else {
			addNode(sid, sid, "missing", "", 0)
		}
	}
	// Then add isolated recent obs (no edges).
	for sid, o := range bySync {
		addNode(sid, o.Title, o.Type, o.Project, o.ID)
	}

	families := collectFamilies(recent)

	if family != "" {
		nodes, edges = filterGraphByFamily(nodes, edges, family, recent)
	}

	payload := graphPayload{
		Project:  project,
		Family:   family,
		Families: families,
		Nodes:    nodes,
		Edges:    edges,
	}
	payload.Stats.Observations = len(nodes)
	payload.Stats.Relations = len(edges)

	w.Header().Set("Content-Type", "application/json")
	enc := jsonEncoder(w)
	if err := enc.Encode(payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ── Diff ──────────────────────────────────────────────────────────────────

func (s *Server) diff(w http.ResponseWriter, r *http.Request) {
	a, errA := strconv.ParseInt(r.URL.Query().Get("a"), 10, 64)
	b, errB := strconv.ParseInt(r.URL.Query().Get("b"), 10, 64)
	if errA != nil || errB != nil {
		s.renderError(w, r, "Invalid a or b id", errors.New("expected integer ids"))
		return
	}
	oa, err := s.C.GetObservation(r.Context(), a)
	if err != nil {
		s.renderError(w, r, fmt.Sprintf("Failed to load #%d", a), err)
		return
	}
	ob, err := s.C.GetObservation(r.Context(), b)
	if err != nil {
		s.renderError(w, r, fmt.Sprintf("Failed to load #%d", b), err)
		return
	}

	data := s.baseData(r, nil)
	data["Title"] = fmt.Sprintf("Diff #%d ↔ #%d", a, b)
	data["A"] = oa
	data["B"] = ob
	data["Lines"] = lineDiff(oa.Content, ob.Content)
	s.render(w, "diff.html.tmpl", data)
}

type diffLine struct {
	Sign string // " ", "-", "+"
	Text string
}

// lineDiff is a minimal LCS-based line diff; good enough for short observation
// content. Not optimised for huge inputs.
func lineDiff(a, b string) []diffLine {
	la := strings.Split(a, "\n")
	lb := strings.Split(b, "\n")
	n, m := len(la), len(lb)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if la[i] == lb[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out []diffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case la[i] == lb[j]:
			out = append(out, diffLine{" ", la[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, diffLine{"-", la[i]})
			i++
		default:
			out = append(out, diffLine{"+", lb[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffLine{"-", la[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffLine{"+", lb[j]})
	}
	return out
}

// ── Obsidian vault export ─────────────────────────────────────────────────
// engram has obsidian-export as a CLI command, not an HTTP route. We surface
// a form that writes a Markdown vault directly from the UI by walking
// observations through /search + /observations/recent, formatting each as
// frontmatter+body, and dropping into a user-supplied directory.

func (s *Server) exportVaultPage(w http.ResponseWriter, r *http.Request) {
	data := s.baseData(r, nil)
	data["Title"] = "Export to Markdown vault"
	data["DefaultProject"] = currentProject(r)
	s.render(w, "export_vault.html.tmpl", data)
}

func (s *Server) exportVaultSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	vault := strings.TrimSpace(r.FormValue("vault"))
	project := strings.TrimSpace(r.FormValue("project"))
	if vault == "" {
		s.renderError(w, r, "vault path is required", errors.New("missing vault"))
		return
	}

	limit := atoiOr(r.FormValue("limit"), 1000, maxScanLimit)
	obs, err := s.C.RecentObservations(r.Context(), project, "", limit, 0)
	if err != nil {
		s.renderError(w, r, "Failed to load observations", err)
		return
	}

	written, err := writeVault(vault, obs)
	if err != nil {
		s.renderError(w, r, "Vault write failed", err)
		return
	}

	data := s.baseData(r, nil)
	data["Title"] = "Export complete"
	data["Vault"] = vault
	data["Written"] = written
	data["Total"] = len(obs)
	s.render(w, "export_vault_done.html.tmpl", data)
}

// ─── render helpers ──────────────────────────────────────────────────────────

func (s *Server) render(w http.ResponseWriter, name string, data map[string]any) {
	tpl := s.pages[name]
	if tpl == nil {
		http.Error(w, "unknown template: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "template execute: "+err.Error(), http.StatusInternalServerError)
	}
}

// renderFragment renders a single named block from a template file WITHOUT the
// layout — used for HTMX partial responses (e.g. the lazy-loaded /sync
// contributor dropdown). name is the file under templates/; block is the
// {{define}} name to execute.
func (s *Server) renderFragment(w http.ResponseWriter, name, block string, data map[string]any) {
	tpl := s.pages[name]
	if tpl == nil {
		http.Error(w, "unknown template: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, block, data); err != nil {
		http.Error(w, "template execute: "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	tpl := s.pages["error.html.tmpl"]
	if tpl == nil {
		http.Error(w, msg+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	data := s.baseData(r, nil)
	data["Title"] = "Error"
	data["Message"] = msg
	data["Detail"] = err.Error()
	_ = tpl.ExecuteTemplate(w, "layout", data)
}

// atoiOr parses a positive query parameter, falling back to def.
//
// max is a ceiling, not a suggestion: these values reach engram as a row limit
// and, worse, reach make() as a capacity. `?days=500000000` used to allocate
// half a billion structs before any work started. A query string is attacker
// input on a server with no authentication, so every one of these has a roof.
func atoiOr(s string, def, max int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func atoiOrZero(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// snippet truncates to n characters, not n bytes.
//
// len() and s[:n] are byte operations, so on accented text or an emoji — both
// routine in these observations — the cut landed mid-rune and produced invalid
// UTF-8. That reached goldmark and the template as a replacement character
// right before the ellipsis: a visible glitch whose cause is invisible.
func snippet(s string, n int) string {
	if len(s) <= n { // fast path: ASCII of that length can never be longer
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func escape(s string) string { return url.QueryEscape(s) }

func writeVault(vault string, obs []client.Observation) (int, error) {
	if err := os.MkdirAll(vault, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir vault: %w", err)
	}
	written := 0
	for _, o := range obs {
		dir := vault
		if o.Project != "" {
			dir = filepath.Join(vault, slug(o.Project))
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return written, err
		}
		name := fmt.Sprintf("%d-%s.md", o.ID, slug(o.Title))
		path := filepath.Join(dir, name)
		body := obsidianBody(o)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", path, err)
		}
		written++
	}
	return written, nil
}

func obsidianBody(o client.Observation) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "id: %d\n", o.ID)
	fmt.Fprintf(&b, "type: %s\n", o.Type)
	fmt.Fprintf(&b, "title: %q\n", o.Title)
	if o.Project != "" {
		fmt.Fprintf(&b, "project: %s\n", o.Project)
	}
	if o.Scope != "" {
		fmt.Fprintf(&b, "scope: %s\n", o.Scope)
	}
	if o.TopicKey != "" {
		fmt.Fprintf(&b, "topic_key: %s\n", o.TopicKey)
	}
	fmt.Fprintf(&b, "created_at: %s\n", o.CreatedAt)
	if o.UpdatedAt != "" {
		fmt.Fprintf(&b, "updated_at: %s\n", o.UpdatedAt)
	}
	if o.RevisionCount > 0 {
		fmt.Fprintf(&b, "revision_count: %d\n", o.RevisionCount)
	}
	b.WriteString("tags:\n")
	if o.Type != "" {
		fmt.Fprintf(&b, "  - engram/%s\n", o.Type)
	}
	if o.Project != "" {
		fmt.Fprintf(&b, "  - project/%s\n", slug(o.Project))
	}
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# %s\n\n%s\n", o.Title, o.Content)
	return b.String()
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "untitled"
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func sortStrings(s []string) { sort.Strings(s) }
func sortTopicKeysByKey(k []topicKey) {
	sort.Slice(k, func(i, j int) bool { return k[i].Key < k[j].Key })
}
func jsonEncoder(w http.ResponseWriter) *json.Encoder {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e
}
func csvWriter(w http.ResponseWriter) *csv.Writer { return csv.NewWriter(w) }
