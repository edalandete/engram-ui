package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/edalandete/engram-ui/internal/gitbridge"
)

// Shared-repo panel: publish a project's memories to a git repository and pull
// other people's back, as an alternative to the engram cloud server. See
// internal/gitbridge for why the format suits git and what the transport does
// not carry.

// shareRefreshTimeout bounds the fast-forward performed while rendering the
// panel. Much shorter than cliCtx on purpose: this one sits in the path of a
// page load, so an unreachable remote must degrade to a stale table within
// seconds instead of leaving the tab spinning. Publish and pull keep the
// generous budget — they are deliberate actions, and losing one halfway is
// worse than waiting.
const shareRefreshTimeout = 20 * time.Second

func (s *Server) sharePage(w http.ResponseWriter, r *http.Request) {
	s.renderShare(w, r, nil)
}

func (s *Server) renderShare(w http.ResponseWriter, r *http.Request, result *syncResult) {
	project := currentProject(r)

	data := s.baseData(r, nil)
	data["Title"] = "Engram Shared"
	data["SyncProject"] = project
	data["Result"] = result
	// Both are compared with gt in the template, so they must always be a
	// number: a missing map key is nil and blows up the render.
	data["PendingConflicts"] = 0
	data["UnappliedCommits"] = 0

	if s.Share == nil {
		data["ShareUnavailable"] = true
		s.render(w, "share.html.tmpl", data)
		return
	}

	data["RepoDir"] = s.Share.Dir()
	// Always a string: the template compares it with eq, which fails on nil.
	data["ProjectRepo"] = ""

	repos := s.Share.List()
	// Fast-forward before scanning. A clone is otherwise only as fresh as this
	// machine's last publish or pull, so a teammate's commit would never show
	// up here — and the table looking unchanged is precisely what stops you
	// from pulling it. GET only: every action below already fast-forwards as
	// part of its own work, and a second round trip just to render its banner
	// would make the button feel slow. A failure is not fatal — the scan still
	// renders, flagged as possibly behind.
	refreshErrs := make([]error, len(repos))
	if r.Method == http.MethodGet {
		ctx, cancel := context.WithTimeout(r.Context(), shareRefreshTimeout)
		defer cancel()
		// IfStale, not Refresh: the fast-forward is ~2.7 s against a real
		// remote. In parallel: each clone has its own lock, and one remote
		// after another would multiply that wait by the number of repos.
		var wg sync.WaitGroup
		for i, b := range repos {
			wg.Go(func() { refreshErrs[i] = b.RefreshIfStale(ctx) })
		}
		wg.Wait()
	}

	cur := s.Share.For(project)
	views := make([]shareRepo, 0, len(repos))
	for i, b := range repos {
		v := shareRepo{ID: b.ID(), URL: b.RemoteURL(r.Context()), Dir: b.Dir(), Size: humanBytes(b.Size())}
		if refreshErrs[i] != nil {
			v.RefreshErr = refreshErrs[i].Error()
		}
		// Discovery is a directory scan of the clone: no index file.
		projects, err := b.Projects(r.Context())
		if err != nil {
			v.ProjectsErr = err.Error()
		}
		v.Projects = projects
		views = append(views, v)

		if b != cur {
			continue
		}
		data["ProjectRepo"] = v.ID
		data["ProjectRepoURL"] = v.URL
		if err == nil {
			here := containsProject(projects, project)
			data["PublishedHere"] = here
			// Computed here rather than as `not .PublishedHere` in the
			// template, because those are not the same question. The key is
			// absent when the clone could not be scanned at all, and the
			// template must not announce "nobody has ever published this"
			// on the strength of a directory listing that failed.
			data["NeverPublished"] = !here
		}
		// Unlike the cloud panel this needs no lazy loading — the manifest is
		// already on disk, so there is nothing slow to defer.
		if users, err := b.Contributors(r.Context(), project); err == nil {
			data["Contributors"] = users
		}
		data["FirstPublishHere"] = b.FirstPublishHere(project)
		data["UnappliedCommits"] = b.UnappliedCommits(project)
		data["Quarantined"] = b.Quarantined(project)
	}
	data["Repos"] = views

	// Same gate as cloud push: publishing a project with unresolved conflicts
	// would replicate contradictory memories to everyone else.
	if project != "" {
		if stats, err := s.C.ConflictStats(r.Context(), project); err == nil {
			data["PendingConflicts"] = stats.ByJudgmentStatus["pending"]
		}
	}

	s.render(w, "share.html.tmpl", data)
}

// shareRepo is one connected repository as the panel shows it.
type shareRepo struct {
	ID, URL, Dir, Size      string
	RefreshErr, ProjectsErr string
	Projects                []gitbridge.ProjectInfo
}

// shareRepoFor resolves the repository a form acts on: the one it names, else
// the one the project publishes to. nil when neither is connected.
func (s *Server) shareRepoFor(r *http.Request, project string) *gitbridge.Bridge {
	if id := strings.TrimSpace(r.FormValue("repo")); id != "" {
		return s.Share.Get(id)
	}
	return s.Share.For(project)
}

// humanBytes renders a size the way a person reads one. Returns "" for zero so
// the template can drop the whole clause rather than print "0 B".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		if n <= 0 {
			return ""
		}
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func containsProject(projects []gitbridge.ProjectInfo, project string) bool {
	if project == "" {
		return false
	}
	want := gitbridge.ProjectDirName(project)
	for _, p := range projects {
		if p.Name == want {
			return true
		}
	}
	return false
}

func (s *Server) requireShare(w http.ResponseWriter, r *http.Request) bool {
	if s.Share == nil {
		s.renderError(w, r, "Engram Shared unavailable",
			errors.New("shared-repo sync requires the embedded/managed engram binary"))
		return false
	}
	return true
}

// shareSetup clones the repository that will hold the shared memories. The
// clone doubles as the configuration: there is no settings file that could
// disagree with what is on disk. Credentials are the user's own git setup.
func (s *Server) shareSetup(w http.ResponseWriter, r *http.Request) {
	if !s.requireShare(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	url := strings.TrimSpace(r.FormValue("repo"))
	if url == "" {
		s.renderError(w, r, "Repository required", errors.New("enter the git URL of the shared repository"))
		return
	}

	ctx, cancel := cliCtx(r.Context())
	defer cancel()
	res := s.Share.Add(ctx, url)
	out := &syncResult{Action: "share-setup", OK: !res.Failed(), Output: res.Output}
	if res.Failed() {
		out.Message = "Could not connect to " + url
		if out.Output == "" {
			out.Output = res.Err.Error()
		}
	} else {
		out.Message = "Connected to " + url
	}
	s.renderShare(w, r, out)
}

// shareDisconnect deletes one local clone. Nothing is lost: published memories
// stay in the remote, imported ones stay in the local database. Projects
// assigned to it go back to unassigned.
func (s *Server) shareDisconnect(w http.ResponseWriter, r *http.Request) {
	if !s.requireShare(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	out := &syncResult{Action: "share-disconnect", OK: true, Message: "Disconnected from the shared repository"}
	if err := s.Share.Remove(strings.TrimSpace(r.FormValue("repo"))); err != nil {
		out.OK = false
		out.Message = "Could not remove the local clone"
		out.Output = err.Error()
	}
	s.renderShare(w, r, out)
}

// shareAssign sets which repository the current project publishes to.
func (s *Server) shareAssign(w http.ResponseWriter, r *http.Request) {
	if !s.requireShare(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	project := currentProject(r)
	if project == "" {
		s.renderError(w, r, "Pick a project first", errors.New("assigning a repository requires a project — use the project switcher"))
		return
	}
	id := strings.TrimSpace(r.FormValue("repo"))
	out := &syncResult{Action: "share-assign", OK: true}
	if err := s.Share.Assign(project, id); err != nil {
		out.OK = false
		out.Message = "Could not assign a repository to " + project
		out.Output = err.Error()
	} else {
		out.Message = project + " now publishes to " + s.Share.Get(id).RemoteURL(r.Context())
	}
	// The author index is read from the project's repository.
	s.invalidateAuthors(project)
	s.renderShare(w, r, out)
}

// sharePush publishes the current project. gitbridge always fast-forwards
// before exporting — the order is not optional, see its package comment — and
// decides for itself whether a full re-export is warranted. There used to be a
// second "Republish everything" button for that; it asked the user a question
// about an export watermark they cannot see.
func (s *Server) sharePush(w http.ResponseWriter, r *http.Request) {
	s.publish(w, r)
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	if !s.requireCLI(w, r) || !s.requireShare(w, r) {
		return
	}
	project := currentProject(r)
	if project == "" {
		s.renderError(w, r, "Pick a project first", errors.New("publishing requires a project — use the project switcher"))
		return
	}

	if stats, err := s.C.ConflictStats(r.Context(), project); err == nil {
		if pending := stats.ByJudgmentStatus["pending"]; pending > 0 {
			s.renderShare(w, r, &syncResult{
				Action:       "share-push",
				Blocked:      true,
				PendingCount: pending,
				Message: fmt.Sprintf("Publish blocked — %s has %d unresolved conflict(s). Resolve them first.",
					project, pending),
			})
			return
		}
	}

	b := s.Share.For(project)
	if b == nil {
		s.renderShare(w, r, &syncResult{
			Action:  "share-push",
			Blocked: true,
			Message: "Publish blocked — pick the repository " + project + " publishes to first.",
		})
		return
	}

	// No re-export guard here on purpose: gitbridge.publish refuses one after
	// its fast-forward, which is the only point where the manifest is current.
	// The disabled button and the warning on this page are the courtesy; that
	// is the guarantee.
	ctx, cancel := cliCtx(r.Context())
	defer cancel()
	s.renderShare(w, r, pushResultFrom(b.Push(ctx, project)))
}

// sharePull imports one project from the repo. The project comes from the form
// rather than the switcher, so you can pull something you don't work on yet.
func (s *Server) sharePull(w http.ResponseWriter, r *http.Request) {
	if !s.requireCLI(w, r) || !s.requireShare(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	project := strings.TrimSpace(r.FormValue("project"))
	if project == "" {
		project = currentProject(r)
	}
	if project == "" {
		s.renderError(w, r, "Pick a project first", errors.New("pulling requires a project"))
		return
	}
	b := s.shareRepoFor(r, project)
	if b == nil {
		s.renderError(w, r, "No repository", errors.New("that repository is not connected"))
		return
	}

	ctx, cancel := cliCtx(r.Context())
	defer cancel()
	res := b.Pull(ctx, project)
	s.invalidateAuthors(project)
	s.renderShare(w, r, sharePullResultFrom(res))
}

// sharePullSelective imports only one contributor's chunks. Partial by design,
// and partial in a way worth knowing about: see gitbridge.PullSelective.
func (s *Server) sharePullSelective(w http.ResponseWriter, r *http.Request) {
	if !s.requireCLI(w, r) || !s.requireShare(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	project := strings.TrimSpace(r.FormValue("project"))
	if project == "" {
		project = currentProject(r)
	}
	if project == "" {
		s.renderError(w, r, "Pick a project first", errors.New("selective pull requires a project"))
		return
	}
	// Same two-mode form as the cloud panel: a radio picks "everything" or "one
	// contributor", and the dropdown only means anything in the second case.
	// An empty created_by is "everyone" down in gitbridge.FilterChunks, so
	// total mode just clears it.
	createdBy := strings.TrimSpace(r.FormValue("created_by"))
	if strings.TrimSpace(r.FormValue("mode")) == "total" {
		createdBy = ""
	} else if createdBy == "" {
		s.renderError(w, r, "Pick a contributor", errors.New("a by-contributor pull needs a created_by value"))
		return
	}
	b := s.shareRepoFor(r, project)
	if b == nil {
		s.renderError(w, r, "No repository", errors.New("pick the repository this project publishes to first"))
		return
	}

	ctx, cancel := cliCtx(r.Context())
	defer cancel()
	res := b.PullSelective(ctx, project, createdBy)
	s.invalidateAuthors(project)
	s.renderShare(w, r, sharePullResultFrom(res))
}

// pushResultFrom adapts a publish outcome into the banner. "Nothing new" is a
// success, not a failure — but it carries a warning when it looks like the
// adoption trap rather than an ordinary no-op.
func pushResultFrom(res gitbridge.PushResult) *syncResult {
	// A refusal is not a failure: nothing was attempted and nothing is broken,
	// so it gets the warning banner and its own words rather than
	// "Publish failed during <stage>".
	if res.Blocked {
		return &syncResult{Action: "share-push", Blocked: true, Message: res.Err.Error(), Output: findingsText(res.Findings)}
	}
	out := &syncResult{Action: "share-push", OK: res.Err == nil, Output: res.Output}
	if res.Err != nil {
		stage := res.Stage
		if stage == "" {
			stage = "publish"
		}
		out.Message = "Publish failed during " + stage + ": " + res.Err.Error()
		return out
	}
	switch {
	case res.Pushed:
		out.Message = "Published " + res.Project + " to the shared repository"
	case res.Warn != "":
		out.Message = "Nothing was exported for " + res.Project
		out.Output = joinNonEmpty(res.Output, res.Warn)
	default:
		out.Message = "Nothing new to publish for " + res.Project
	}
	return out
}

// findingsText lists what the screening refused, one observation per line.
func findingsText(findings []gitbridge.Finding) string {
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		line := f.ID
		if f.Title != "" {
			line += " · " + f.Title
		}
		line += " — " + f.Reason
		if f.Excerpt != "" {
			line += "\n    " + f.Excerpt
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// sharePublishAnyway publishes the commit the screening held back, once the
// user has judged its findings a false alarm.
func (s *Server) sharePublishAnyway(w http.ResponseWriter, r *http.Request) {
	if !s.requireCLI(w, r) || !s.requireShare(w, r) {
		return
	}
	project := currentProject(r)
	if project == "" {
		s.renderError(w, r, "Pick a project first", errors.New("publishing requires a project — use the project switcher"))
		return
	}
	b := s.Share.For(project)
	if b == nil {
		s.renderShare(w, r, &syncResult{
			Action:  "share-push",
			Blocked: true,
			Message: "Publish blocked — pick the repository " + project + " publishes to first.",
		})
		return
	}
	ctx, cancel := cliCtx(r.Context())
	defer cancel()
	s.renderShare(w, r, pushResultFrom(b.PublishQuarantined(ctx, project)))
}

// sharePullResultFrom adapts a retrieve into the banner. It says "commit" where
// gitbridge says "chunk": the shared-repo screen speaks the user's vocabulary,
// see the note at the top of share.html.tmpl.
func sharePullResultFrom(res gitbridge.PullResult) *syncResult {
	who := res.CreatedBy
	if who == "" {
		who = "all contributors"
	}
	out := &syncResult{Action: "share-pull", OK: res.Err == nil, Output: res.Output}
	if res.Err != nil {
		out.Message = "Pull failed: " + res.Err.Error()
		if out.Output == "" {
			out.Output = res.Err.Error()
		}
		return out
	}
	if res.ChunksMatched == 0 {
		out.Message = fmt.Sprintf("No commits matched %s in %q (%d in the repository)", who, res.Project, res.ChunksTotal)
		return out
	}
	out.Message = fmt.Sprintf("Pulled %d/%d commit(s) for %s in %q",
		res.ChunksMatched, res.ChunksTotal, who, res.Project)
	return out
}

func joinNonEmpty(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}
