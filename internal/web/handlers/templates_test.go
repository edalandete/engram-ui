package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/edalandete/engram-ui/internal/client"
	"github.com/edalandete/engram-ui/internal/cloudbridge"
	"github.com/edalandete/engram-ui/internal/gitbridge"
	"github.com/edalandete/engram-ui/internal/web"
)

// TestTemplatesParse is a smoke test: every content template must parse against
// the shared base (layout + _pagination). Catches template syntax errors and
// references to undefined blocks — the kind of break that otherwise only
// surfaces at request time.
func TestTemplatesParse(t *testing.T) {
	base := New(nil, nil, nil, nil, "") // funcs + base templates; nil deps unused for parsing

	entries, err := fs.Glob(web.TemplatesFS, "templates/*.html.tmpl")
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	for _, name := range entries {
		short := strings.TrimPrefix(name, "templates/")
		t.Run(short, func(t *testing.T) {
			tpl, err := base.tpl.Clone()
			if err != nil {
				t.Fatalf("clone: %v", err)
			}
			if _, err := tpl.ParseFS(web.TemplatesFS, name); err != nil {
				t.Fatalf("parse %s: %v", short, err)
			}
		})
	}
}

// TestConflictsTemplateExplainsItself renders the Conflicts screen and checks
// that the vocabulary is translated: the screen is useless if it just repeats
// engram's internal verbs at someone who has never read engram's source.
func TestConflictsTemplateExplainsItself(t *testing.T) {
	base := New(nil, nil, nil, nil, "")

	data := map[string]any{
		"Title": "Conflicts", "Theme": "dark", "CurrentProject": "infinity-core",
		"Projects": []string{"infinity-core"}, "Path": "/conflicts",
		"Stats": &client.ConflictStats{
			ByJudgmentStatus: map[string]int{"pending": 3, "judged": 40},
			Deferred:         2,
			Dead:             1,
		},
		"Statuses": []string{"pending", "judged", "orphaned", "ignored"},
		"Status":   "pending", "RelationFilter": "",
		"RelationFilters": canonicalRelations,
		"Page": &client.ConflictsPage{Relations: []client.ConflictListItem{
			{ID: 7, Relation: "conflicts_with", JudgmentStatus: "pending",
				SourceTitle: "A", TargetTitle: "B", CreatedAt: "2026-08-20 10:00:00"},
		}},
		"Limit": 25, "Offset": 0,
		"HasNext": false, "ShowPagination": false,
	}

	tpl, err := base.tpl.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tpl.ParseFS(web.TemplatesFS, "templates/conflicts.html.tmpl"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// Every verb the filter offers must come with its meaning.
	for _, verb := range canonicalRelations {
		if !strings.Contains(out, relationHelp(verb)) {
			t.Errorf("no explanation rendered for relation %q", verb)
		}
	}
	// Statuses are labelled, not dumped raw.
	if !strings.Contains(out, "Pending review") {
		t.Error("expected the human label for the pending status")
	}
	// The counter must survive being looked up by key rather than ranged over.
	if !strings.Contains(out, ">3<") {
		t.Error("expected the pending count (3) to render")
	}
}

// The scan is the one operation here that runs for minutes, so the two things
// that make it survivable get a check: the slot really is single (a second
// submit while one runs must not start another), and the card renders in all
// three of its states — never run, running, finished.
func TestScanSlotIsSingleAndRenders(t *testing.T) {
	s := New(nil, nil, nil, nil, "")

	if s.scanState() != nil {
		t.Fatal("a fresh server reports a scan")
	}
	if !s.claimScan("infinity-core") {
		t.Fatal("the first scan was refused")
	}
	if s.claimScan("other") {
		t.Error("a second scan started on top of the first")
	}
	if job := s.scanState(); job == nil || job.Done || job.Project != "infinity-core" {
		t.Fatalf("running state = %+v, want an unfinished infinity-core scan", job)
	}
	// The copy handed to templates must not be the live job: the scan
	// goroutine writes the original while a render reads it.
	if s.scanState() == s.scan {
		t.Error("scanState returned the live job, not a copy")
	}

	s.finishScan(&client.ScanResult{Project: "infinity-core", Inserted: 3}, nil)
	job := s.scanState()
	if job == nil || !job.Done || job.Result.Inserted != 3 {
		t.Fatalf("finished state = %+v, want Done with 3 inserted", job)
	}
	if job.Elapsed() == "" {
		t.Error("no elapsed time on a finished scan")
	}
	if !s.claimScan("other") {
		t.Error("the slot stayed locked after the scan finished")
	}
	s.finishScan(nil, errors.New("engram said no"))
	if job := s.scanState(); job.Err != "engram said no" {
		t.Errorf("failure not recorded: %+v", job)
	}
	// Our deadline is not engram's: a timeout must say the scan carries on.
	s.claimScan("other")
	s.finishScan(nil, fmt.Errorf("engram /conflicts/scan: %w", context.DeadlineExceeded))
	if job := s.scanState(); !strings.Contains(job.Err, "engram is still scanning") {
		t.Errorf("timeout does not warn the scan is still running: %q", job.Err)
	}

	// Render the card standalone, the way GET /conflicts/scan/status does.
	for _, c := range []struct {
		name string
		scan *scanJob
		want string
	}{
		{"never run", nil, "Scan <code>infinity-core</code>"},
		{"running", &scanJob{Project: "infinity-core", Started: time.Now()},
			`hx-get="/conflicts/scan/status"`},
		{"finished", &scanJob{Project: "infinity-core", Started: time.Now(), Ended: time.Now(), Done: true,
			Result: &client.ScanResult{Inserted: 3}}, "3 added"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := s.pages["conflicts.html.tmpl"].ExecuteTemplate(&buf, "scan-status", map[string]any{
				"Scan": c.scan, "CurrentProject": "infinity-core", "AgentCLI": "opencode",
			})
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			out := buf.String()
			if !strings.Contains(out, c.want) {
				t.Errorf("card is missing %q:\n%s", c.want, out)
			}
			// A running scan offers no way to start a second one.
			running := c.scan != nil && !c.scan.Done
			if got := strings.Contains(out, `action="/conflicts/scan"`); got == running {
				t.Errorf("form present = %v while running = %v", got, running)
			}
			// And only a running scan polls: a finished one that kept its
			// hx-trigger would hammer the endpoint forever.
			if got := strings.Contains(out, "hx-trigger"); got != running {
				t.Errorf("polling = %v, want %v", got, running)
			}
		})
	}
}

// TestShareTemplateExecutes renders the shared-repo screen across its states:
// git missing, connected to nothing, and connected with projects, contributors
// and a result banner. The screen is heavily branched, so executing it is the
// cheapest way to catch a mis-nested {{end}} or a renamed field.
func TestShareTemplateExecutes(t *testing.T) {
	base := New(nil, nil, nil, nil, "")

	common := func(extra map[string]any) map[string]any {
		data := map[string]any{
			"Title": "Engram Shared", "Theme": "dark", "CurrentProject": "infinity-core",
			"Projects": []string{"infinity-core"}, "Path": "/share",
			"SyncProject": "infinity-core", "PendingConflicts": 0, "Result": nil,
			"UnappliedCommits": 0,
		}
		for k, v := range extra {
			data[k] = v
		}
		return data
	}

	cases := []map[string]any{
		common(map[string]any{"ShareUnavailable": true}),
		common(map[string]any{"RepoDir": `C:\Users\x\.engram`}),
		common(map[string]any{
			"RepoDir": "/home/x/.engram", "ProjectRepo": "share-repo",
			"Repos": []shareRepo{{ID: "share-repo", URL: "git@github.com:acme/mem.git", Dir: "/home/x/.engram/share-repo",
				Projects: []gitbridge.ProjectInfo{
					{Name: "infinity-core", Chunks: 3, Memories: 42,
						Contributors: []string{"ana", "luis"}, LastUpdate: "2026-01-02T03:04:05Z",
						NewChunks: 1, NewMemories: 12, Silent: []string{"luis"}},
				}}},
			"PublishedHere": true,
			"Contributors": []gitbridge.UserCount{
				{User: "ana", Chunks: 2, Memories: 30},
				{User: "luis", Chunks: 1, Memories: 12},
			},
			"Result": &syncResult{Action: "share-push", OK: true, Message: "Published", Output: "Created chunk abc"},
		}),
		common(map[string]any{ // blocked by conflicts
			"Repos": oneRepo(), "ProjectRepo": "share-repo",
			"PendingConflicts": 2,
			"Result":           &syncResult{Action: "share-push", Blocked: true, PendingCount: 2, Message: "Publish blocked"},
		}),
	}
	for i, data := range cases {
		tpl, err := base.tpl.Clone()
		if err != nil {
			t.Fatalf("clone: %v", err)
		}
		if _, err := tpl.ParseFS(web.TemplatesFS, "templates/share.html.tmpl"); err != nil {
			t.Fatalf("parse: %v", err)
		}
		var buf bytes.Buffer
		if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
			t.Fatalf("case %d execute: %v", i, err)
		}
		if i == 2 && !strings.Contains(buf.String(), "ana") {
			t.Error("expected the contributor list to render")
		}
	}
}

func oneRepo() []shareRepo {
	return []shareRepo{{ID: "share-repo", URL: "git@github.com:acme/mem.git", Dir: "/tmp/share-repo"}}
}

// With several repositories each one gets its own table, every pull says which
// clone it reads from, and a project nobody has assigned cannot be published:
// guessing the repository would hand the project to whoever can read it.
func TestShareWithSeveralRepositories(t *testing.T) {
	base := New(nil, nil, nil, nil, "")
	repos := []shareRepo{
		{ID: "share-repo", URL: "git@github.com:acme/team-a.git",
			Projects: []gitbridge.ProjectInfo{{Name: "alpha", NewChunks: 1, NewMemories: 3}}},
		{ID: "share-repo-team-b", URL: "git@github.com:acme/team-b.git",
			Projects: []gitbridge.ProjectInfo{{Name: "beta", NewChunks: 1, NewMemories: 5}}},
	}
	render := func(t *testing.T, projectRepo string) string {
		t.Helper()
		tpl, err := base.tpl.Clone()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tpl.ParseFS(web.TemplatesFS, "templates/share.html.tmpl"); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := tpl.ExecuteTemplate(&buf, "layout", map[string]any{
			"Title": "Engram Shared", "Theme": "dark", "CurrentProject": "alpha",
			"Projects": []string{"alpha"}, "Path": "/share",
			"SyncProject": "alpha", "PendingConflicts": 0, "UnappliedCommits": 0, "Result": nil,
			"Repos": repos, "ProjectRepo": projectRepo,
		}); err != nil {
			t.Fatalf("execute: %v", err)
		}
		return buf.String()
	}

	out := render(t, "share-repo-team-b")
	for _, want := range []string{
		"Projects in the repository · <code>git@github.com:acme/team-a.git",
		"Projects in the repository · <code>git@github.com:acme/team-b.git",
		`name="repo" value="share-repo-team-b"`,
		`action="/share/assign"`,
		`<option value="share-repo-team-b" selected>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "has no repository yet") {
		t.Error("asked for a repository the project already has")
	}

	out = render(t, "")
	if !strings.Contains(out, "has no repository yet") {
		t.Error("no warning for a project with no repository")
	}
	if !strings.Contains(out, "disabled>↑ Publish now") {
		t.Error("Publish enabled for a project with no repository")
	}
}

// TestObservationsAuthorBadge renders the observations list with an author
// index and asserts the cloud-provenance badge appears only for the observation
// whose sync_id is in the index (cloud-origin), not for the local one.
func TestObservationsAuthorBadge(t *testing.T) {
	base := New(nil, nil, nil, nil, "")

	data := map[string]any{
		"Title": "Observations", "Theme": "dark", "CurrentProject": "p",
		"Projects": []string{"p"}, "Path": "/observations",
		"Observations": []client.Observation{
			{ID: 1, SyncID: "obs-cloud", Type: "manual", Title: "FROM-CLOUD", Project: "p"},
			{ID: 2, SyncID: "obs-local", Type: "manual", Title: "LOCAL-ONLY", Project: "p"},
			{ID: 3, SyncID: "", Type: "manual", Title: "NO-SYNCID", Project: "p"},
		},
		"Authors": map[string]string{"obs-cloud": "alice"},
		// The screen now carries the search form too, so the render has to be
		// exercised with the filter state it merged in.
		"Query": "auth bug", "Scope": "project", "Type": "bugfix",
		"Scopes": canonicalScopes, "Types": canonicalTypes,
		"CreatedBy": "alice", "Contributors": []string{"alice", "bob"},
		"MatchedCount": 1, "ScannedCount": 3, "ScanCapped": false,
		"HasNext":        false,
		"ShowPagination": false,
		"Limit":          25,
	}

	tpl, err := base.tpl.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tpl.ParseFS(web.TemplatesFS, "templates/observations.html.tmpl"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "☁ alice") {
		t.Error("expected cloud badge '☁ alice' for the cloud-origin observation")
	}
	// The local obs (#2) and no-syncid obs (#3) must not get a badge — only one
	// badge total should be present.
	if n := strings.Count(out, "badge-cloud"); n != 1 {
		t.Errorf("expected exactly 1 cloud badge, got %d", n)
	}
	// The merged screen must carry the search form: this is the page that
	// replaced /search, so losing the query box would lose the feature.
	for _, want := range []string{`name="q"`, `name="scope"`, `name="type"`, `name="created_by"`, "auth bug"} {
		if !strings.Contains(out, want) {
			t.Errorf("merged Observations screen is missing %s", want)
		}
	}
}

// TestNarrow covers the filtering engram cannot do for us. The sharp edges: an
// observation with no sync_id was never published and belongs to nobody; type
// and scope must combine with the author instead of replacing it (a query-less
// listing gets no server-side filtering at all, which is what made "type
// bugfix" show discoveries); and paging happens after filtering, over the
// matches rather than over the scan.
func TestNarrow(t *testing.T) {
	authors := map[string]string{"s1": "dev-c", "s2": "dev-a", "s3": "dev-c"}
	obs := []client.Observation{
		{ID: 1, SyncID: "s1", Type: "bugfix", Scope: "project"},
		{ID: 2, SyncID: "s2", Type: "bugfix", Scope: "project"},
		{ID: 3, SyncID: "s3", Type: "discovery", Scope: "personal"},
		{ID: 4, SyncID: "", Type: "bugfix", Scope: "project"}, // local, never published
	}

	got := narrow(obs, "", "", "dev-c", "", authors)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Fatalf("author filter = %+v, want observations 1 and 3", got)
	}
	if n := len(narrow(obs, "", "", "nobody", "", authors)); n != 0 {
		t.Errorf("unknown contributor matched %d observations, want 0", n)
	}
	if n := len(narrow(obs, "", "", "dev-c", "", nil)); n != 0 {
		t.Errorf("no author index matched %d observations, want 0", n)
	}

	// Type alone, the case that was silently ignored.
	if got := narrow(obs, "", "bugfix", "", "", authors); len(got) != 3 {
		t.Errorf("type filter kept %d, want the 3 bugfixes: %+v", len(got), got)
	}
	if got := narrow(obs, "", "BugFix", "", "", authors); len(got) != 3 {
		t.Errorf("type filter should be case-insensitive, kept %d", len(got))
	}
	// Filters combine rather than override.
	if got := narrow(obs, "", "bugfix", "dev-c", "", authors); len(got) != 1 || got[0].ID != 1 {
		t.Errorf("type+author = %+v, want only observation 1", got)
	}
	if got := narrow(obs, "personal", "", "", "", authors); len(got) != 1 || got[0].ID != 3 {
		t.Errorf("scope filter = %+v, want only observation 3", got)
	}
	if n := len(narrow(obs, "", "", "", "", authors)); n != len(obs) {
		t.Errorf("no filters kept %d, want all %d", n, len(obs))
	}

	got = narrow(obs, "", "", "dev-c", "", authors)

	if p := page(got, 1, 25); len(p) != 1 || p[0].ID != 3 {
		t.Errorf("page(offset=1) = %+v, want just observation 3", p)
	}
	if p := page(got, 99, 25); p != nil {
		t.Errorf("offset past the end = %+v, want nil", p)
	}

	if names := distinctAuthors(authors); len(names) != 2 || names[0] != "dev-a" || names[1] != "dev-c" {
		t.Errorf("distinctAuthors = %v, want [dev-a dev-c]", names)
	}
}

// TestSyncTemplateExecutes renders the cloud-sync screen across its states:
// no cloud server, a pull-selective result, a push blocked by conflicts, and a
// project with pending conflicts (Push disabled). The contributor dropdown is
// lazy-loaded as a separate fragment (see TestPullUsersFragmentExecutes).
func TestSyncTemplateExecutes(t *testing.T) {
	base := New(nil, nil, nil, nil, "")

	cases := []map[string]any{
		{ // no cloud server
			"Title": "Engram Cloud", "Theme": "dark", "CurrentProject": "infinity-core",
			"Projects": []string{"infinity-core"}, "Path": "/sync",
			"SyncProject": "infinity-core", "CloudServer": "", "Result": nil,
			"PendingConflicts": 0,
		},
		{ // cloud server + a pull-selective result, no conflicts
			"Title": "Engram Cloud", "Theme": "light", "CurrentProject": "infinity-core",
			"Projects": []string{"infinity-core"}, "Path": "/sync",
			"SyncProject": "infinity-core", "CloudServer": "http://127.0.0.1:18080",
			"PendingConflicts": 0,
			"Result":           &syncResult{Action: "pull-selective", OK: true, Message: "Pulled 3/4 chunk(s) for alice"},
		},
		{ // push blocked by conflicts
			"Title": "Engram Cloud", "Theme": "dark", "CurrentProject": "infinity-core",
			"Projects": []string{"infinity-core"}, "Path": "/sync",
			"SyncProject": "infinity-core", "CloudServer": "http://127.0.0.1:18080",
			"PendingConflicts": 5,
			"Result":           &syncResult{Action: "push", Blocked: true, PendingCount: 5, Message: "Push blocked — infinity-core has 5 unresolved conflict(s). Resolve them before pushing."},
		},
	}
	for i, data := range cases {
		tpl, err := base.tpl.Clone()
		if err != nil {
			t.Fatalf("clone: %v", err)
		}
		if _, err := tpl.ParseFS(web.TemplatesFS, "templates/sync.html.tmpl"); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if err := tpl.ExecuteTemplate(io.Discard, "layout", data); err != nil {
			t.Fatalf("case %d execute: %v", i, err)
		}
	}
}

// Both sync panels hide everything behind the one setting that makes them work:
// no cloud server, no cloud controls; no repository, no shared-repo controls.
// A page full of buttons that cannot do anything teaches nobody what is
// missing, and disabling them is not the same as not drawing them — a disabled
// Push still looks like a thing you nearly have.
//
// Rendered rather than eyeballed because the guard is one `{{if}}` around a
// hundred lines: an edit that lands a card outside it looks harmless in a diff.
func TestPanelsHideTheirControlsUntilConfigured(t *testing.T) {
	base := New(nil, nil, nil, nil, "")

	render := func(t *testing.T, name string, data map[string]any) string {
		t.Helper()
		tpl, err := base.tpl.Clone()
		if err != nil {
			t.Fatalf("clone: %v", err)
		}
		if _, err := tpl.ParseFS(web.TemplatesFS, "templates/"+name); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		var buf bytes.Buffer
		if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
			t.Fatalf("execute %s: %v", name, err)
		}
		return buf.String()
	}

	// Match on markup, not on bare words. The prose on these panels mentions
	// "publish", "cloud" and "up to date" in passing, so a substring check on
	// the word alone reports on whichever sentence happens to be nearby — it
	// has produced a false pass and a false failure already.
	cloudGated := []string{"<h3>Enrollment", "<h3>Background sync status", "<h3>Manual sync", "<h3>Selective pull", "<h3>Repair"}
	shareGated := []string{"<h3>Connected", "<h3>Publish", "<h3>Projects in the repository", "<h3>Selective pull", "<h3>Known limits"}

	base4 := func(title, path string) map[string]any {
		return map[string]any{
			"Title": title, "Theme": "dark", "CurrentProject": "infinity-core",
			"Projects": []string{"infinity-core"}, "Path": path,
			"SyncProject": "infinity-core", "PendingConflicts": 0, "Result": nil,
			"UnappliedCommits": 0,
		}
	}

	t.Run("cloud without a server", func(t *testing.T) {
		data := base4("Engram Cloud", "/sync")
		data["CloudServer"] = ""
		out := render(t, "sync.html.tmpl", data)

		if !strings.Contains(out, "<h3>Cloud server") {
			t.Error("the one box that unblocks the panel must always render")
		}
		if !strings.Contains(out, "No cloud server configured") {
			t.Error("expected the warning to say why the rest is missing")
		}
		for _, h := range cloudGated {
			if strings.Contains(out, h) {
				t.Errorf("%q rendered without a cloud server", h)
			}
		}
	})

	t.Run("cloud with a server", func(t *testing.T) {
		data := base4("Engram Cloud", "/sync")
		data["CloudServer"] = "http://127.0.0.1:18080"
		out := render(t, "sync.html.tmpl", data)

		for _, h := range append(cloudGated, "<h3>Cloud server") {
			if !strings.Contains(out, h) {
				t.Errorf("%q missing once a cloud server is set", h)
			}
		}
		// Configured first, controls after: the setting reads as the prerequisite.
		if i, j := strings.Index(out, "<h3>Cloud server"), strings.Index(out, "<h3>Manual sync"); i > j {
			t.Errorf("Cloud server box renders at %d, after Manual sync at %d", i, j)
		}
	})

	t.Run("share without a repository", func(t *testing.T) {
		data := base4("Engram Shared", "/share")
		data["RepoDir"] = `C:\Users\x\AppData\Local\engram-ui\shared`
		out := render(t, "share.html.tmpl", data)

		if !strings.Contains(out, "<h3>Connect a repository") {
			t.Error("the connect form must always render")
		}
		for _, h := range shareGated {
			if strings.Contains(out, h) {
				t.Errorf("%q rendered without a repository", h)
			}
		}
	})

	t.Run("share with a repository", func(t *testing.T) {
		data := base4("Engram Shared", "/share")
		data["Repos"] = []shareRepo{{ID: "share-repo", URL: "git@github.com:acme/engram-memories.git",
			Dir: `C:\Users\x\.engram\share-repo`, Size: "24.0 MiB"}}
		data["ProjectRepo"] = "share-repo"
		out := render(t, "share.html.tmpl", data)

		for _, h := range shareGated {
			if !strings.Contains(out, h) {
				t.Errorf("%q missing once a repository is connected", h)
			}
		}
		if strings.Contains(out, "<h3>Connect a repository") {
			t.Error("still offering to connect while already connected")
		}
		if i, j := strings.Index(out, "<h3>Connected"), strings.Index(out, "<h3>Publish"); i > j {
			t.Errorf("Connected box renders at %d, after Publish at %d", i, j)
		}
	})

	// git missing is a third state: not even the connect form is actionable.
	t.Run("share without git", func(t *testing.T) {
		data := base4("Engram Shared", "/share")
		data["ShareUnavailable"] = true
		out := render(t, "share.html.tmpl", data)

		if !strings.Contains(out, "No <code>git</code> on PATH") {
			t.Error("expected the missing-git explanation")
		}
		for _, h := range append(shareGated, "<h3>Connect a repository") {
			if strings.Contains(out, h) {
				t.Errorf("%q rendered with no git available", h)
			}
		}
	})
}

// The shared-repo table offers a Pull only when observations are actually
// missing, never merely because a commit is unrecorded.
//
// The two disagree routinely: a file export is not filtered by author, so your
// own commits re-publish everything you had pulled from everyone else. After a
// disconnect/reconnect the per-clone ledger is gone, every teammate commit
// counts as unrecorded, and yet nothing is missing. Gating on chunks rendered
// "Pull 0 new observations" — and pressing it was harmful, since engram's
// import upserts with no newer-wins and an older commit overwrites newer text.
func TestSharedRepoOffersPullOnlyForMissingObservations(t *testing.T) {
	base := New(nil, nil, nil, nil, "")

	render := func(t *testing.T, projects []gitbridge.ProjectInfo) string {
		t.Helper()
		tpl, err := base.tpl.Clone()
		if err != nil {
			t.Fatalf("clone: %v", err)
		}
		if _, err := tpl.ParseFS(web.TemplatesFS, "templates/share.html.tmpl"); err != nil {
			t.Fatalf("parse: %v", err)
		}
		// Mirror the row into the Publish card, the way the handler does: both
		// come from the same scan, so a test where they disagree would be
		// testing a state that cannot happen.
		behind := 0
		if len(projects) > 0 {
			behind = projects[0].NewChunks
		}
		var buf bytes.Buffer
		err = tpl.ExecuteTemplate(&buf, "layout", map[string]any{
			"Title": "Engram Shared", "Theme": "dark", "CurrentProject": "infinity-core",
			"Projects": []string{"infinity-core"}, "Path": "/share",
			"SyncProject": "infinity-core", "PendingConflicts": 0, "Result": nil,
			"Repos":       []shareRepo{{ID: "share-repo", URL: "git@github.com:acme/mem.git", Projects: projects}},
			"ProjectRepo": "share-repo", "UnappliedCommits": behind,
		})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		return buf.String()
	}

	// Unapplied commits carrying no absent observation. Two ways to get here:
	// a teammate edited something you already hold, or you reconnected the
	// repository and this clone forgot what it had applied. Must never render
	// "Pull 0 new observations", and must never claim to be up to date — an
	// edit is invisible to a sync_id count, so hiding the button here is how a
	// teammate's correction silently fails to arrive.
	t.Run("unapplied commits, no absent observations", func(t *testing.T) {
		out := render(t, []gitbridge.ProjectInfo{{
			Name: "infinity-core", Chunks: 4, Memories: 332,
			NewChunks: 2, NewMemories: 0,
			Contributors: []string{"dev-a", "dev-c"},
		}})
		if strings.Contains(out, "Pull 0 new observation") {
			t.Error(`rendered "Pull 0 new observations": counting commits, labelling observations`)
		}
		if strings.Contains(out, "✓ up to date") {
			t.Error("claimed up to date with 2 unapplied commits: edits would never arrive")
		}
		if !strings.Contains(out, "Pull 2 commits") {
			t.Error("expected the commit-count button for edits-only work")
		}
	})

	// One publish button, no second "Republish everything": whether a publish
	// re-exports the whole project is decided from the export watermark, which
	// the user cannot see and was never in a position to judge.
	t.Run("only one publish button exists", func(t *testing.T) {
		out := render(t, []gitbridge.ProjectInfo{{Name: "infinity-core"}})
		if strings.Contains(out, "Republish") {
			t.Error("the second publish button is back")
		}
		if strings.Contains(out, "/share/republish") {
			t.Error("a form still posts to the removed republish route")
		}
		if !strings.Contains(out, "Publish now") {
			t.Error("the one publish button is missing")
		}
	})

	// Being behind is what makes a full re-export destructive: it sends this
	// machine's copy of every observation and the import takes the last writer
	// without comparing dates, so a correction made while you were behind is
	// undone for the whole team. The page must say so; gitbridge.publish is
	// what actually refuses.
	t.Run("being behind is explained on the publish card", func(t *testing.T) {
		out := render(t, []gitbridge.ProjectInfo{{
			Name: "infinity-core", NewChunks: 2, NewMemories: 0,
		}})
		if !strings.Contains(out, "not applied on this machine") {
			t.Error("expected an explanation of why a full re-export is unsafe right now")
		}
	})

	// A block has two causes now, and only the conflicts one has somewhere to
	// send you. The banner used to hardcode the link, so the other cause
	// rendered "Review 0 conflict(s)" pointing at an empty list.
	t.Run("re-export block does not offer a conflicts link", func(t *testing.T) {
		tpl, err := base.tpl.Clone()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tpl.ParseFS(web.TemplatesFS, "templates/share.html.tmpl"); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		err = tpl.ExecuteTemplate(&buf, "layout", map[string]any{
			"Title": "Engram Shared", "Theme": "dark", "CurrentProject": "infinity-core",
			"Projects": []string{"infinity-core"}, "Path": "/share",
			"SyncProject": "infinity-core", "PendingConflicts": 0, "UnappliedCommits": 2,
			"Repos": oneRepo(), "ProjectRepo": "share-repo",
			"Result": &syncResult{Action: "share-push", Blocked: true,
				Message: "Re-export blocked — 2 commit(s) in the repository are not applied here."},
		})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "Review 0 conflict") {
			t.Error(`rendered "Review 0 conflict(s)": the banner assumes every block is a conflict`)
		}
		if !strings.Contains(out, "Re-export blocked") {
			t.Error("the block message itself did not render")
		}
	})

	t.Run("nothing at all outstanding", func(t *testing.T) {
		out := render(t, []gitbridge.ProjectInfo{{
			Name: "infinity-core", Chunks: 4, Memories: 332,
			NewChunks: 0, NewMemories: 0,
		}})
		if !strings.Contains(out, "✓ up to date") {
			t.Error("a project with nothing outstanding must read as up to date")
		}
		if strings.Contains(out, "↓ Pull") {
			t.Error("offered a pull with nothing to pull")
		}
	})

	t.Run("observations actually missing", func(t *testing.T) {
		out := render(t, []gitbridge.ProjectInfo{{
			Name: "infinity-core", Chunks: 4, Memories: 332,
			NewChunks: 1, NewMemories: 49,
			Contributors: []string{"dev-a", "dev-c"},
		}})
		if !strings.Contains(out, "Pull 49 new observations") {
			t.Error("expected the pull button labelled with the missing observation count")
		}
		if strings.Contains(out, "✓ up to date") {
			t.Error("claimed up to date while 49 observations are missing")
		}
	})

	// Singular/plural, because "Pull 1 new observations" is the kind of thing
	// that makes a user distrust the number next to it.
	t.Run("exactly one missing", func(t *testing.T) {
		out := render(t, []gitbridge.ProjectInfo{{
			Name: "infinity-core", NewChunks: 1, NewMemories: 1,
		}})
		if !strings.Contains(out, "Pull 1 new observation<") {
			t.Error("expected the singular form for a single missing observation")
		}
	})
}

// TestPullUsersFragmentExecutes renders the lazy-loaded contributor dropdown
// fragment with populated users, no users, and a fetch error.
func TestPullUsersFragmentExecutes(t *testing.T) {
	base := New(nil, nil, nil, nil, "")

	cases := []map[string]any{
		{ // populated contributors
			"SyncProject": "infinity-core", "CloudServer": "http://127.0.0.1:18080",
			"PullUsers": []cloudbridge.UserCount{
				{User: "alice", Chunks: 3, Obs: 12},
				{User: "bob", Chunks: 1, Obs: 4},
			},
			"CloudTotalObs": 16,
		},
		{ // no contributors found
			"SyncProject": "infinity-core", "CloudServer": "http://127.0.0.1:18080",
		},
		{ // fetch failed
			"SyncProject": "infinity-core", "CloudServer": "http://127.0.0.1:18080",
			"PullUsersErr": "unauthorized (check cloud token)",
		},
	}
	for i, data := range cases {
		tpl, err := base.tpl.Clone()
		if err != nil {
			t.Fatalf("clone: %v", err)
		}
		if _, err := tpl.ParseFS(web.TemplatesFS, "templates/_pull_users.html.tmpl"); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if err := tpl.ExecuteTemplate(io.Discard, "pull-users", data); err != nil {
			t.Fatalf("case %d execute: %v", i, err)
		}
	}
}

// Sizes are read at a glance, so the unit boundaries have to be right — an
// off-by-one in the loop turns 5 MiB into 5120 KiB.
func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, ""},
		{-1, ""},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{5 * 1024 * 1024, "5.0 MiB"},
		{3 * 1024 * 1024 * 1024, "3.0 GiB"},
	} {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The heatmap has always linked to /observations?since=YYYY-MM-DD and nothing
// ever read the parameter, so clicking a day silently showed the whole list.
// A day is matched as a string prefix of created_at, which is why the format
// has to be checked rather than trusted.
func TestNarrowBySinceMatchesOneDay(t *testing.T) {
	obs := []client.Observation{
		{ID: 1, CreatedAt: "2026-08-20 09:15:00"},
		{ID: 2, CreatedAt: "2026-08-20 23:59:59"},
		{ID: 3, CreatedAt: "2026-08-21 00:00:01"},
		{ID: 4, CreatedAt: "2026-08-19 12:00:00"},
	}
	got := narrow(obs, "", "", "", "2026-08-20", nil)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("since=2026-08-20 selected %+v, want observations 1 and 2", got)
	}
	if n := len(narrow(obs, "", "", "", "", nil)); n != len(obs) {
		t.Errorf("an empty since must filter nothing, got %d of %d", n, len(obs))
	}
	// A loose value would match more than a day: "2026-08" is every August.
	if !validDay("2026-08-20") {
		t.Error("a well-formed day was rejected")
	}
	for _, bad := range []string{"2026-08", "2026-8-20", "26-08-20", "2026/08/20", "", "xxxx-xx-xx", "2026-08-20 09"} {
		if validDay(bad) {
			t.Errorf("validDay(%q) accepted a value that is not one day", bad)
		}
	}
}

// Deferred scripts execute in DOCUMENT ORDER, and every per-page script lives
// inside <main>. So the shared ones have to appear before it — from the bottom
// of <body> they would run last, and a page script asking app.js for the
// shared activity fetch got an undefined function and threw before drawing
// anything. The dashboard updated on a project switch and the heatmap did not.
func TestSharedScriptsLoadBeforePageScripts(t *testing.T) {
	raw, err := web.TemplatesFS.ReadFile("templates/layout.html.tmpl")
	if err != nil {
		t.Fatalf("read layout: %v", err)
	}
	layout := string(raw)

	appJS := strings.Index(layout, `src="/static/js/app.js"`)
	htmxJS := strings.Index(layout, `src="/static/js/htmx.min.js"`)
	// Anchored on the injection point, not on "<main>": the word appears in
	// the comments too, and the first match was one of them — this test failed
	// against correct code the first time it ran.
	content := strings.Index(layout, `{{template "content" .}}`)
	if appJS < 0 || htmxJS < 0 || content < 0 {
		t.Fatal("layout no longer has the pieces this test is about")
	}
	if appJS > content {
		t.Error("app.js loads after the page content: scripts inside it run first and cannot use app.js")
	}
	if htmxJS > content {
		t.Error("htmx loads after the page content")
	}

	// And the page scripts really are inside main, which is what makes the
	// ordering matter rather than being a stylistic preference.
	dash, err := web.TemplatesFS.ReadFile("templates/dashboard.html.tmpl")
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	if !strings.Contains(string(dash), "/static/js/heatmap.js") {
		t.Skip("dashboard no longer loads a page script")
	}
}

// The Publish card has to distinguish three situations that look alike from
// the outside, and a fourth where it must say nothing at all.
func TestPublishCardStatesAreExclusive(t *testing.T) {
	base := New(nil, nil, nil, nil, "")

	render := func(t *testing.T, extra map[string]any) string {
		t.Helper()
		tpl, err := base.tpl.Clone()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tpl.ParseFS(web.TemplatesFS, "templates/share.html.tmpl"); err != nil {
			t.Fatal(err)
		}
		data := map[string]any{
			"Title": "Engram Shared", "Theme": "dark", "CurrentProject": "infinity-core",
			"Projects": []string{"infinity-core"}, "Path": "/share",
			"SyncProject": "infinity-core", "PendingConflicts": 0, "UnappliedCommits": 0,
			"Result": nil, "Repos": oneRepo(), "ProjectRepo": "share-repo",
		}
		for k, v := range extra {
			data[k] = v
		}
		var buf bytes.Buffer
		if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
			t.Fatalf("execute: %v", err)
		}
		return buf.String()
	}

	const never = "has never been published to this repository"
	const noRecord = "has no record of publishing"

	t.Run("nobody has ever published it", func(t *testing.T) {
		out := render(t, map[string]any{"NeverPublished": true})
		if !strings.Contains(out, never) {
			t.Error("no warning that the project is absent from the repository")
		}
		if strings.Contains(out, noRecord) {
			t.Error("also claimed this machine has no record: the two are exclusive")
		}
	})

	t.Run("in the repository, but not from this machine", func(t *testing.T) {
		out := render(t, map[string]any{"FirstPublishHere": true})
		if !strings.Contains(out, noRecord) {
			t.Error("no warning that this publish re-sends everything")
		}
		if strings.Contains(out, never) {
			t.Error("claimed nobody had published a project that is in the repository")
		}
	})

	t.Run("published and recorded", func(t *testing.T) {
		out := render(t, map[string]any{"PublishedHere": true})
		if strings.Contains(out, never) || strings.Contains(out, noRecord) {
			t.Error("warned about a project that is published and recorded")
		}
		if !strings.Contains(out, "Already in the repository") {
			t.Error("lost the positive confirmation")
		}
	})

	// The one that matters most: NeverPublished is absent when the clone could
	// not be scanned. "We could not look" must never render as "it is not
	// there" — that would push someone into a full publish on bad information.
	t.Run("scan failed: says nothing either way", func(t *testing.T) {
		out := render(t, map[string]any{"Repos": []shareRepo{{ID: "share-repo", URL: "x", ProjectsErr: "permission denied"}}})
		if strings.Contains(out, never) {
			t.Error("claimed the project was never published, on a scan that failed")
		}
		if !strings.Contains(out, "permission denied") {
			t.Error("swallowed the scan error instead of showing it")
		}
	})
}
