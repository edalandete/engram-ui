package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/edalandete/engram-ui/internal/cloudbridge"
	"github.com/edalandete/engram-ui/internal/cloudclient"
	"github.com/edalandete/engram-ui/internal/engramcli"
)

// syncResult carries the outcome of a CLI action into the panel render so the
// user sees the command output inline (push/pull can print multi-line summaries).
type syncResult struct {
	Action  string // "push" | "pull" | "pull-selective" | "enroll" | "config" | "resync"
	OK      bool
	Output  string
	Message string // short human summary for the banner
	// Blocked marks a push refused before it ran because the project has
	// unresolved conflicts. PendingCount carries how many, so the banner can
	// link to /conflicts and tell the user what to clear first.
	Blocked      bool
	PendingCount int
}

// syncPage renders the cloud-sync control panel: live status pill (from the
// local HTTP /sync/status), an enroll form, and push/pull triggers. The
// mutating actions POST back here and re-render with the captured CLI output.
// Cloud server config + backup live on /settings.
func (s *Server) syncPage(w http.ResponseWriter, r *http.Request) {
	s.renderSync(w, r, nil)
}

// syncConfig persists the cloud server URL. It lives on the Engram Cloud screen
// rather than in Settings: it is the address every button on that screen talks
// to, and configuring it two pages away from where it is used was a detour.
func (s *Server) syncConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireCLI(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}
	server := strings.TrimSpace(r.FormValue("server"))
	if server == "" {
		s.renderError(w, r, "Server URL required", errors.New("cloud server URL cannot be empty"))
		return
	}
	ctx, cancel := cliCtx(r.Context())
	defer cancel()
	res := s.CLI.SetServer(ctx, server)
	s.renderSync(w, r, resultFrom("config", res, "Set cloud server to "+server))
}

// syncUnlink forgets the configured server. Not a CLI call: engram has no way
// to express "no server" (see engramcli.ClearServer).
func (s *Server) syncUnlink(w http.ResponseWriter, r *http.Request) {
	out := &syncResult{Action: "config", OK: true, Message: "Cloud server cleared — push and pull are off until you set one again"}
	if err := engramcli.ClearServer(); err != nil {
		out.OK = false
		out.Message = "Could not clear the cloud server"
		out.Output = err.Error()
	}
	s.renderSync(w, r, out)
}

func (s *Server) renderSync(w http.ResponseWriter, r *http.Request, result *syncResult) {
	project := currentProject(r)

	data := s.baseData(r, nil)
	data["Title"] = "Engram Cloud"
	data["SyncProject"] = project
	data["Result"] = result
	data["CloudServer"] = engramcli.CurrentServer()
	data["PendingConflicts"] = 0

	// Status pill is best-effort: engram returns enabled:false when background
	// autosync isn't configured, which is the normal case for manual sync.
	if status, err := s.C.SyncStatus(r.Context(), project); err == nil {
		data["Status"] = status
	} else {
		data["StatusErr"] = err.Error()
	}

	// Pending-conflict count gates Push (see syncPush). Surface it here too so
	// the user sees the block reason before clicking. Best-effort; local call.
	if project != "" {
		if stats, err := s.C.ConflictStats(r.Context(), project); err == nil {
			data["PendingConflicts"] = stats.ByJudgmentStatus["pending"]
		}
	}

	// The selective-pull contributor list is NOT computed here — building it
	// downloads every cloud chunk, which made page entry slow. The template
	// renders a placeholder that lazy-loads /sync/pull-users via HTMX after
	// paint; see syncPullUsers.
	s.render(w, "sync.html.tmpl", data)
}

// syncPullUsers renders just the selective-pull contributor dropdown as an HTMX
// fragment. Split out of the page render because aggregating per-contributor obs
// counts downloads every cloud chunk (seconds for a large project) — doing it
// inline blocked /sync entry. Now the page paints instantly and this fills in.
func (s *Server) syncPullUsers(w http.ResponseWriter, r *http.Request) {
	project := currentProject(r)
	data := map[string]any{
		"SyncProject": project,
		"CloudServer": engramcli.CurrentServer(),
	}
	users, err := s.pullUsersForProject(r.Context(), project)
	if err != nil {
		data["PullUsersErr"] = err.Error()
	} else {
		data["PullUsers"] = users
		// Per-pusher Obs are disjoint (first-wins attribution), so their sum is
		// the project's true unique-observation total on the cloud.
		total := 0
		for _, u := range users {
			total += u.Obs
		}
		data["CloudTotalObs"] = total
	}
	s.renderFragment(w, "_pull_users.html.tmpl", "pull-users", data)
}

// pullUsersForProject returns the per-contributor chunk/obs counts for the
// selective-pull dropdown, cached with a TTL (push/pull invalidate it). Real
// obs counts download + dedup every chunk payload; on that error it falls back
// to manifest-only chunk counts. Returns (nil, nil) when there's nothing to
// query (no server or no project).
func (s *Server) pullUsersForProject(ctx context.Context, project string) ([]cloudbridge.UserCount, error) {
	server := engramcli.CurrentServer()
	if server == "" || project == "" {
		return nil, nil // nothing to query; UI shows only "Pull total" guidance
	}

	s.pullUsersMu.Lock()
	if e, ok := s.pullUsersCache[project]; ok && time.Since(e.fetched) < pullUsersTTL {
		users := e.users
		s.pullUsersMu.Unlock()
		return users, nil
	}
	s.pullUsersMu.Unlock()

	cc, err := cloudclient.New(server, s.cloudToken)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	users, err := s.Bridge.UsersWithObsCounts(cctx, cc, project)
	if err != nil {
		// Fall back to manifest-only chunk counts (single HTTP call, no obs
		// count). Don't cache the fallback — a transient chunk-fetch failure
		// shouldn't pin the cheap counts for the whole TTL.
		manifest, mErr := cc.ReadManifest(cctx, project)
		if mErr != nil {
			return nil, err
		}
		return cloudbridge.Users(manifest), nil
	}

	s.pullUsersMu.Lock()
	s.pullUsersCache[project] = pullUsersEntry{users: users, fetched: time.Now()}
	s.pullUsersMu.Unlock()
	return users, nil
}

// cliCtx bounds a CLI action. Push/pull hit the network and can take a while;
// enroll/config are local and fast. 5 min is generous headroom for a push.
func cliCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 5*time.Minute)
}

func (s *Server) requireCLI(w http.ResponseWriter, r *http.Request) bool {
	if s.CLI == nil {
		s.renderError(w, r, "Engram Cloud unavailable",
			errors.New("engram CLI not available — sync requires the embedded/managed engram binary"))
		return false
	}
	return true
}

func (s *Server) syncPush(w http.ResponseWriter, r *http.Request) {
	if !s.requireCLI(w, r) {
		return
	}
	project := currentProject(r)
	if project == "" {
		s.renderError(w, r, "Pick a project first", errors.New("cloud push requires a project — use the project switcher"))
		return
	}

	// Gate: don't push a project with unresolved conflicts — pushing would
	// replicate contradictory memories to the cloud. Best-effort: if the stats
	// call fails we don't block (the push CLI is still the source of truth).
	if stats, err := s.C.ConflictStats(r.Context(), project); err == nil {
		if pending := stats.ByJudgmentStatus["pending"]; pending > 0 {
			s.renderSync(w, r, &syncResult{
				Action:       "push",
				Blocked:      true,
				PendingCount: pending,
				Message: fmt.Sprintf("Push blocked — %s has %d unresolved conflict(s). Resolve them before pushing.",
					project, pending),
			})
			return
		}
	}

	ctx, cancel := cliCtx(r.Context())
	defer cancel()
	res := s.CLI.SyncPush(ctx, project)
	s.invalidateAuthors(project) // pushing this project can add new provenance
	s.renderSync(w, r, resultFrom("push", res, "Pushed "+project+" to cloud"))
}

func (s *Server) syncPull(w http.ResponseWriter, r *http.Request) {
	if !s.requireCLI(w, r) {
		return
	}
	project := currentProject(r)
	if project == "" {
		s.renderError(w, r, "Pick a project first", errors.New("cloud pull requires a project — use the project switcher"))
		return
	}
	ctx, cancel := cliCtx(r.Context())
	defer cancel()
	res := s.CLI.SyncPull(ctx, project)
	s.invalidateAuthors(project)
	s.renderSync(w, r, resultFrom("pull", res, "Pulled "+project+" from cloud"))
}

func (s *Server) syncEnroll(w http.ResponseWriter, r *http.Request) {
	if !s.requireCLI(w, r) {
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
		s.renderError(w, r, "Project required", errors.New("enroll needs a project name"))
		return
	}
	ctx, cancel := cliCtx(r.Context())
	defer cancel()
	res := s.CLI.Enroll(ctx, project)
	s.renderSync(w, r, resultFrom("enroll", res, "Enrolled "+project))
}

// syncResync clears the project's cloud sync bookkeeping so the next push
// re-uploads everything local. Repairs divergence after a server reset or
// interrupted pushes. Local-only and non-destructive (no observations deleted).
func (s *Server) syncResync(w http.ResponseWriter, r *http.Request) {
	if !s.requireCLI(w, r) {
		return
	}
	project := currentProject(r)
	if project == "" {
		s.renderError(w, r, "Pick a project first", errors.New("resync requires a project — use the project switcher"))
		return
	}
	ctx, cancel := cliCtx(r.Context())
	defer cancel()
	res := s.CLI.Resync(ctx, project)
	s.renderSync(w, r, resultFrom("resync", res, "Reset cloud sync state for "+project+" — now Push to re-upload everything"))
}

// syncPullSelective downloads a chosen subset of a project's cloud chunks
// (mode=total → all, mode=user → only created_by=<user>) and applies them via a
// local import — engram's cloud import is all-or-nothing, so this routes around
// it through cloudbridge. Bypasses cloud-mode enrollment/health (local import
// has none); fine for read+apply.
func (s *Server) syncPullSelective(w http.ResponseWriter, r *http.Request) {
	if !s.requireCLI(w, r) {
		return
	}
	project := currentProject(r)
	if project == "" {
		s.renderError(w, r, "Pick a project first", errors.New("selective pull requires a project — use the project switcher"))
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, "Invalid form", err)
		return
	}

	mode := strings.TrimSpace(r.FormValue("mode"))
	createdBy := strings.TrimSpace(r.FormValue("created_by"))
	if mode == "total" {
		createdBy = "" // empty filter = all chunks
	} else if createdBy == "" {
		s.renderError(w, r, "Pick a user", errors.New("by-user pull needs a created_by value"))
		return
	}

	server := engramcli.CurrentServer()
	if server == "" {
		s.renderError(w, r, "No cloud server", errors.New("set a cloud server in Settings before pulling"))
		return
	}
	cc, err := cloudclient.New(server, s.cloudToken)
	if err != nil {
		s.renderError(w, r, "Invalid cloud server", err)
		return
	}

	ctx, cancel := cliCtx(r.Context())
	defer cancel()
	res := s.Bridge.Pull(ctx, cc, project, createdBy)
	s.invalidateAuthors(project)
	s.renderSync(w, r, pullResultFrom(res))
}

// pullResultFrom adapts a cloudbridge.PullResult into the syncResult banner.
func pullResultFrom(res cloudbridge.PullResult) *syncResult {
	who := res.CreatedBy
	if who == "" {
		who = "all users"
	}
	out := &syncResult{Action: "pull-selective", OK: res.Err == nil, Output: res.Output}
	if res.Err != nil {
		out.Message = "Selective pull failed — see output below"
		if out.Output == "" {
			out.Message = "Selective pull failed: " + res.Err.Error()
			out.Output = res.Err.Error()
		}
		return out
	}
	if res.ChunksMatched == 0 {
		out.Message = fmt.Sprintf("No chunks matched %s in %q (%d total on cloud)", who, res.Project, res.ChunksTotal)
		return out
	}
	out.Message = fmt.Sprintf("Pulled %d/%d chunk(s) for %s in %q", res.ChunksMatched, res.ChunksTotal, who, res.Project)
	return out
}

func resultFrom(action string, res engramcli.Result, okMsg string) *syncResult {
	out := &syncResult{Action: action, OK: !res.Failed(), Output: res.Output}
	if res.Failed() {
		// res.Err is usually a bare "exit status 1"; the useful detail is in
		// Output. Give a readable headline and let the output block carry the
		// specifics. When there's no captured output, fall back to the error.
		out.Message = action + " failed — see output below"
		if out.Output == "" {
			out.Message = action + " failed: " + res.Err.Error()
			out.Output = res.Err.Error()
		}
	} else {
		out.Message = okMsg
	}
	return out
}
