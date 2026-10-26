package gitbridge

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edalandete/engram-ui/internal/gitcli"
)

// TestReconcileMergesConcurrentManifests drives the one path where a bug would
// silently lose memories: two people publish the same project at the same time,
// the second push is rejected, and the manifests have to be merged.
//
// Losing an entry here is unrecoverable in practice. engram records every chunk
// it exports in its sync_chunks table under the "local" target, so a dropped
// manifest entry makes the recomputed chunk hash as an already-known duplicate
// and `engram sync` answers "all memories already exported" forever. Hence the
// assertion that all three entries survive, not just that the merge succeeded.
//
// Real git, real bare remote, no engram binary needed: reconcile only uses git.
func TestReconcileMergesConcurrentManifests(t *testing.T) {
	ctx := context.Background()
	git := gitcli.New()
	if err := git.Available(ctx); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	// Identity via env so the test never depends on the machine's git config.
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	t.Setenv("ENGRAM_DATA_DIR", filepath.Join(root, "data"))

	bare := filepath.Join(root, "origin.git")
	mustGit(t, "", "init", "--quiet", "--bare", bare)

	b := New(nil, git)
	if res := b.Setup(ctx, bare); res.Failed() {
		t.Fatalf("setup: %v\n%s", res.Err, res.Output)
	}

	const project = "shared"
	base := manifestJSON(t, entry("aaa", "ana", "2026-01-01T00:00:00Z"))
	writeManifest(t, b.Dir(), project, base)
	mustGit(t, b.Dir(), "add", "-A")
	mustGit(t, b.Dir(), "commit", "--quiet", "-m", "seed")
	mustGit(t, b.Dir(), "push", "--quiet", "--set-upstream", "origin", branchOf(t, b.Dir()))

	// Someone else publishes first: same project, a new chunk of their own.
	other := filepath.Join(root, "other")
	mustGit(t, "", "clone", "--quiet", bare, other)
	writeManifest(t, other, project, manifestJSON(t,
		entry("aaa", "ana", "2026-01-01T00:00:00Z"),
		entry("bbb", "luis", "2026-01-02T00:00:00Z"),
	))
	mustGit(t, other, "add", "-A")
	mustGit(t, other, "commit", "--quiet", "-m", "luis publishes")
	mustGit(t, other, "push", "--quiet", "origin", "HEAD")

	// We publish concurrently, unaware of the above.
	writeManifest(t, b.Dir(), project, manifestJSON(t,
		entry("aaa", "ana", "2026-01-01T00:00:00Z"),
		entry("ccc", "ana", "2026-01-03T00:00:00Z"),
	))
	mustGit(t, b.Dir(), "add", "-A")
	mustGit(t, b.Dir(), "commit", "--quiet", "-m", "ana publishes")

	if push := git.Push(ctx, b.Dir()); !push.Failed() {
		t.Fatal("expected the concurrent push to be rejected, but it succeeded")
	} else if !gitcli.IsRejected(push) {
		t.Fatalf("push failed for an unexpected reason:\n%s", push.Output)
	}

	if err := b.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if push := git.Push(ctx, b.Dir()); push.Failed() {
		t.Fatalf("push after reconcile: %v\n%s", push.Err, push.Output)
	}

	// Every chunk from both sides must be present, in chronological order.
	m, err := readManifest(filepath.Join(b.Dir(), project, syncDirName, manifestName))
	if err != nil {
		t.Fatalf("read merged manifest: %v", err)
	}
	if got := strings.Join(ids(m.Chunks), ","); got != "aaa,bbb,ccc" {
		t.Errorf("merged manifest = %s, want aaa,bbb,ccc", got)
	}

	// And the remote must agree — a merge that only fixed the worktree would
	// still leave the next person conflicting.
	out := mustGit(t, other, "fetch", "--quiet", "origin")
	_ = out
	mustGit(t, other, "reset", "--quiet", "--hard", "origin/"+branchOf(t, other))
	remote, err := readManifest(filepath.Join(other, project, syncDirName, manifestName))
	if err != nil {
		t.Fatalf("read remote manifest: %v", err)
	}
	if got := strings.Join(ids(remote.Chunks), ","); got != "aaa,bbb,ccc" {
		t.Errorf("published manifest = %s, want aaa,bbb,ccc", got)
	}
}

// TestReconcileAbortsOnUnknownConflict makes sure we never "resolve" a file we
// don't understand. Chunks are content-addressed and can only ever be added, so
// a conflict outside a manifest means something unexpected is in the repo, and
// guessing there could destroy someone's data.
func TestReconcileAbortsOnUnknownConflict(t *testing.T) {
	ctx := context.Background()
	git := gitcli.New()
	if err := git.Available(ctx); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	t.Setenv("ENGRAM_DATA_DIR", filepath.Join(root, "data"))

	bare := filepath.Join(root, "origin.git")
	mustGit(t, "", "init", "--quiet", "--bare", bare)

	b := New(nil, git)
	if res := b.Setup(ctx, bare); res.Failed() {
		t.Fatalf("setup: %v\n%s", res.Err, res.Output)
	}
	readme := filepath.Join(b.Dir(), "README.md")
	mustWrite(t, readme, "base\n")
	mustGit(t, b.Dir(), "add", "-A")
	mustGit(t, b.Dir(), "commit", "--quiet", "-m", "seed")
	mustGit(t, b.Dir(), "push", "--quiet", "--set-upstream", "origin", branchOf(t, b.Dir()))

	other := filepath.Join(root, "other")
	mustGit(t, "", "clone", "--quiet", bare, other)
	mustWrite(t, filepath.Join(other, "README.md"), "theirs\n")
	mustGit(t, other, "add", "-A")
	mustGit(t, other, "commit", "--quiet", "-m", "theirs")
	mustGit(t, other, "push", "--quiet", "origin", "HEAD")

	mustWrite(t, readme, "ours\n")
	mustGit(t, b.Dir(), "add", "-A")
	mustGit(t, b.Dir(), "commit", "--quiet", "-m", "ours")

	err := b.reconcile(ctx)
	if err == nil {
		t.Fatal("expected reconcile to refuse a non-manifest conflict")
	}
	if !strings.Contains(err.Error(), "README.md") {
		t.Errorf("error should name the conflicting file, got: %v", err)
	}
	// The merge must be aborted, not left half-applied.
	if unmerged, _ := git.Unmerged(ctx, b.Dir()); len(unmerged) != 0 {
		t.Errorf("worktree left mid-merge with conflicts: %v", unmerged)
	}
}

// TestRefreshRevealsSomeoneElsesPublish reproduces the bug this method exists
// for: a teammate publishes, and the panel keeps showing yesterday's numbers
// because a directory scan of a clone nobody fetched cannot know. The stale
// half of the assertion matters as much as the fresh half — it is what makes
// the test fail if someone later moves the fetch back out of the read path.
func TestRefreshRevealsSomeoneElsesPublish(t *testing.T) {
	ctx := context.Background()
	git := gitcli.New()
	if err := git.Available(ctx); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	t.Setenv("ENGRAM_DATA_DIR", filepath.Join(root, "data"))

	bare := filepath.Join(root, "origin.git")
	mustGit(t, "", "init", "--quiet", "--bare", bare)

	b := New(nil, git)
	if res := b.Setup(ctx, bare); res.Failed() {
		t.Fatalf("setup: %v\n%s", res.Err, res.Output)
	}

	const project = "infinity-core"
	writeManifest(t, b.Dir(), project, manifestJSON(t, entry("aaa", "dev-a", "2026-08-19T15:41:24Z")))
	mustGit(t, b.Dir(), "add", "-A")
	mustGit(t, b.Dir(), "commit", "--quiet", "-m", "engram: publish "+project)
	mustGit(t, b.Dir(), "push", "--quiet", "--set-upstream", "origin", branchOf(t, b.Dir()))

	// A colleague publishes to the same project from their own machine.
	other := filepath.Join(root, "other")
	mustGit(t, "", "clone", "--quiet", bare, other)
	writeManifest(t, other, project, manifestJSON(t,
		entry("aaa", "dev-a", "2026-08-19T15:41:24Z"),
		entry("bbb", "dev-c", "2026-08-20T09:12:00Z"),
	))
	mustGit(t, other, "add", "-A")
	mustGit(t, other, "commit", "--quiet", "-m", "engram: publish "+project)
	mustGit(t, other, "push", "--quiet", "origin", "HEAD")

	before, err := b.Projects(context.Background())
	if err != nil {
		t.Fatalf("projects before refresh: %v", err)
	}
	if got := before[0].LastUpdate; got != "2026-08-19T15:41:24Z" {
		t.Fatalf("scan without refresh should be stale, got last update %q", got)
	}

	if err := b.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	after, err := b.Projects(context.Background())
	if err != nil {
		t.Fatalf("projects after refresh: %v", err)
	}
	if got := after[0].LastUpdate; got != "2026-08-20T09:12:00Z" {
		t.Errorf("last update after refresh = %q, want the colleague's publish", got)
	}
	if got := strings.Join(after[0].Contributors, ","); got != "dev-a,dev-c" {
		t.Errorf("contributors after refresh = %q, want dev-a,dev-c", got)
	}
}

// TestRefreshOnEmptyRepoIsNotAnError keeps the page-render contract: a repo
// nobody has published to yet must render the "be the first" empty state, not
// a red warning.
func TestRefreshOnEmptyRepoIsNotAnError(t *testing.T) {
	ctx := context.Background()
	git := gitcli.New()
	if err := git.Available(ctx); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	t.Setenv("ENGRAM_DATA_DIR", filepath.Join(root, "data"))
	bare := filepath.Join(root, "origin.git")
	mustGit(t, "", "init", "--quiet", "--bare", bare)

	b := New(nil, git)
	if res := b.Setup(ctx, bare); res.Failed() {
		t.Fatalf("setup: %v\n%s", res.Err, res.Output)
	}
	if err := b.Refresh(ctx); err != nil {
		t.Errorf("refresh on an empty repo = %v, want nil", err)
	}
}

// TestPendingCountsWhatWasNotImported covers the panel's "Here" column: a
// manifest says what the repo holds, never what you have, so the answer comes
// from the ledger. The interesting case is the middle one — a project you have
// partly pulled must read as partly pending, not as done.
func TestPendingCountsWhatWasNotImported(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	t.Setenv("USER", "") // Windows path: engram falls through USER to USERNAME
	t.Setenv("USERNAME", "unrelated")
	b := &Bridge{dir: root}

	const project = "infinity-core"
	writeManifest(t, root, project, manifestJSON(t,
		entry("aaa", "dev-a", "2026-08-19T15:41:24Z"),
		entry("bbb", "dev-c", "2026-08-20T09:12:00Z"),
	))
	writeChunk(t, root, project, "aaa", "obs-1")
	writeChunk(t, root, project, "bbb", "obs-2")

	// Nothing recorded and nothing of ours: everything is pending.
	// Over-reporting is the deliberate bias — see imported.go.
	got := onlyProject(t, b)
	if got.NewChunks != 2 || got.NewMemories != 2 {
		t.Fatalf("fresh clone: NewChunks/NewMemories = %d/%d, want 2/2", got.NewChunks, got.NewMemories)
	}

	// Pull one contributor's chunk.
	b.markImported(project, []string{"aaa"})
	got = onlyProject(t, b)
	if got.NewChunks != 1 {
		t.Errorf("after partial pull: NewChunks = %d, want 1", got.NewChunks)
	}
	users, err := b.Contributors(context.Background(), project)
	if err != nil {
		t.Fatalf("contributors: %v", err)
	}
	for _, u := range users {
		want := 0
		if u.User == "dev-c" {
			want = 1
		}
		if u.New != want {
			t.Errorf("%s.New = %d, want %d", u.User, u.New, want)
		}
	}

	// Pull the rest.
	b.markImported(project, []string{"bbb"})
	if got = onlyProject(t, b); got.NewChunks != 0 {
		t.Errorf("after full pull: NewChunks = %d, want 0", got.NewChunks)
	}

	// A ledger that survives a reload is the whole point: the panel is
	// re-rendered from scratch on every request.
	if got = onlyProject(t, &Bridge{dir: root}); got.NewChunks != 0 {
		t.Errorf("ledger did not persist: NewChunks = %d, want 0", got.NewChunks)
	}
}

// TestOwnChunksAreNeverMissing is the day-one case: a clone with no ledger at
// all must still report the projects you published yourself as complete, and
// flag only the teammate's chunk. Getting this wrong paints every row red and
// makes the column worthless.
func TestOwnChunksAreNeverMissing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	t.Setenv("USER", "")
	t.Setenv("USERNAME", "dev-a")
	b := &Bridge{dir: root}

	writeManifest(t, root, "infinity-cache-configurator", manifestJSON(t,
		entry("aaa", "dev-a", "2026-08-19T15:23:55Z"),
	))
	writeChunk(t, root, "infinity-cache-configurator", "aaa", "obs-1")
	writeManifest(t, root, "infinity-core", manifestJSON(t,
		entry("bbb", "dev-a", "2026-08-19T15:41:24Z"),
		entry("ccc", "dev-c", "2026-08-20T09:05:11Z"),
	))
	writeChunk(t, root, "infinity-core", "bbb", "obs-2")
	writeChunk(t, root, "infinity-core", "ccc", "obs-3")

	projects, err := b.Projects(context.Background())
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(projects))
	}
	if got := projects[0].NewChunks; got != 0 {
		t.Errorf("a project only we published shows %d missing chunk(s), want 0", got)
	}
	if got := projects[1].NewChunks; got != 1 {
		t.Errorf("infinity-core shows %d missing chunk(s), want 1 (the teammate's)", got)
	}
	if got := projects[1].NewMemories; got != 1 {
		t.Errorf("missing memories = %d, want only the teammate's chunk counted", got)
	}
}

// TestAuthorIndexReadsChunks proves attribution can be rebuilt from the clone
// alone. engram keeps no author on an observation, so this mapping is the only
// thing standing between the user and "who wrote this?" — and it has to survive
// the chunk being gzip named .jsonl.gz while holding a single JSON document.
func TestAuthorIndexReadsChunks(t *testing.T) {
	root := t.TempDir()
	b := &Bridge{dir: root}

	const project = "infinity-core"
	writeManifest(t, root, project, manifestJSON(t,
		entry("aaa", "dev-a", "2026-08-19T15:41:24Z"),
		entry("bbb", "dev-c", "2026-08-20T09:05:11Z"),
	))
	writeChunk(t, root, project, "aaa", "obs-1", "obs-2")
	writeChunk(t, root, project, "bbb", "obs-3")

	idx, err := b.AuthorIndex(context.Background(), project)
	if err != nil {
		t.Fatalf("author index: %v", err)
	}
	want := map[string]string{"obs-1": "dev-a", "obs-2": "dev-a", "obs-3": "dev-c"}
	for id, who := range want {
		if idx[id] != who {
			t.Errorf("%s attributed to %q, want %q", id, idx[id], who)
		}
	}
	if len(idx) != len(want) {
		t.Errorf("index has %d entries, want %d: %v", len(idx), len(want), idx)
	}

	// A republish carries the publisher's whole database, other people's
	// memories included. The first publisher must keep the credit, or one
	// re-export would relabel the entire project.
	writeManifest(t, root, project, manifestJSON(t,
		entry("aaa", "dev-a", "2026-08-19T15:41:24Z"),
		entry("ccc", "dev-c", "2026-08-21T10:00:00Z"),
	))
	writeChunk(t, root, project, "ccc", "obs-1", "obs-2", "obs-9") // his re-export includes ours
	idx, err = b.AuthorIndex(context.Background(), project)
	if err != nil {
		t.Fatalf("author index after a republish: %v", err)
	}
	if idx["obs-1"] != "dev-a" || idx["obs-2"] != "dev-a" {
		t.Errorf("a republish stole authorship: %v", idx)
	}
	if idx["obs-9"] != "dev-c" {
		t.Errorf("obs-9 attributed to %q, want dev-c", idx["obs-9"])
	}

	// A chunk listed in the manifest but missing from disk must not sink the
	// whole screen — the rest still gets attributed.
	writeManifest(t, root, project, manifestJSON(t,
		entry("aaa", "dev-a", "2026-08-19T15:41:24Z"),
		entry("gone", "nobody", "2026-08-21T00:00:00Z"),
	))
	idx, err = b.AuthorIndex(context.Background(), project)
	if err != nil {
		t.Fatalf("author index with a missing chunk: %v", err)
	}
	if idx["obs-1"] != "dev-a" || len(idx) != 2 {
		t.Errorf("missing chunk broke the index: %v", idx)
	}
}

// created_by is the OS username of the exporting process, and engram running as
// LocalSystem stamps the Windows machine account. Only then does the panel name
// the committer instead; a real username stays, whatever git config says. And
// "is this chunk mine?" keeps comparing created_by, the only thing the local
// engram can match.
func TestMachineAccountChunksAreCreditedToTheirCommitter(t *testing.T) {
	ctx := context.Background()
	git := gitcli.New()
	if err := git.Available(ctx); err != nil {
		t.Skip("git not available")
	}
	root := filepath.Join(t.TempDir(), "share-repo")
	mustGit(t, "", "init", "--quiet", root)
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	t.Setenv("USER", "")
	t.Setenv("USERNAME", "4A5B6A7CFB1B$") // this machine is the one with the odd engram
	b := &Bridge{dir: root, git: git}

	const project = "infinity-core"
	publish := func(who string, entries ...ChunkEntry) {
		for _, e := range entries {
			writeChunk(t, root, project, e.ID, "obs-"+e.ID)
		}
		writeManifest(t, root, project, manifestJSON(t, entries...))
		t.Setenv("GIT_AUTHOR_NAME", who)
		mustGit(t, root, "add", "-A")
		mustGit(t, root, "commit", "--quiet", "-m", "engram: publish "+project)
	}
	alonso := entry("aaa", "dev-a", "2026-10-06T10:00:00Z")
	oscar := entry("bbb", "4A5B6A7CFB1B$", "2026-10-07T09:29:47Z")
	publish("Rafael Alonso", alonso)
	publish("Jane Doe", alonso, oscar)

	got := onlyProject(t, b)
	if c := strings.Join(got.Contributors, ","); c != "Jane Doe,dev-a" {
		t.Errorf("contributors = %q, want the username, and the committer only for the machine account", c)
	}
	if got.NewChunks != 1 {
		t.Errorf("NewChunks = %d, want 1: our own chunk is recognised by created_by, not by committer", got.NewChunks)
	}
	idx, err := b.AuthorIndex(ctx, project)
	if err != nil {
		t.Fatalf("author index: %v", err)
	}
	if idx["obs-bbb"] != "Jane Doe" {
		t.Errorf("obs-bbb attributed to %q, want Jane Doe", idx["obs-bbb"])
	}
}

// TestStagedViewHidesOtherContributors is the regression test for the bug that
// made the second contributor's publish land zero memories.
//
// engram's file export takes the newest created_at in the manifest of the
// directory it exports into and keeps only rows written after it. Show it a
// teammate's chunk from yesterday and it will export nothing you wrote before
// yesterday — no error, just an empty chunk. Each contributor must therefore be
// handed a manifest containing only their own chunks.
func TestStagedViewHidesOtherContributors(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	t.Setenv("USER", "")
	t.Setenv("USERNAME", "dev-c")
	b := &Bridge{dir: root}

	const project = "infinity-core"
	writeManifest(t, root, project, manifestJSON(t,
		entry("aaa", "dev-a", "2026-08-19T15:41:24Z"),
		entry("bbb", "dev-c", "2026-08-20T09:05:11Z"),
	))

	// A clone with no publishing history stages nothing: watermark zero, full
	// export. This is also what a first publish must do.
	if v := b.stagedView(project); len(v) != 0 {
		t.Fatalf("a clone that never published staged %+v, want nothing", v)
	}

	b.markPublished(project, []string{"bbb"})
	staged := b.stagedView(project)
	if len(staged) != 1 || staged[0].ID != "bbb" {
		t.Fatalf("staged view = %+v, want only the chunk this clone published", staged)
	}
	// The teammate's newer-than-mine timestamp is exactly what would have
	// truncated the export, so assert on the watermark itself.
	for _, e := range staged {
		if e.CreatedBy == "dev-a" {
			t.Errorf("another contributor's chunk leaked into the export watermark: %+v", e)
		}
	}
}

// TestStagedViewIsPerCloneNotPerUsername covers the second machine: engram
// stamps chunks with a username, so a laptop and a workstation used by the same
// person publish under one name. Staging by name would hand this machine the
// other one's timestamp and truncate its export — the original bug, wearing a
// different hat. The ledger lives in this clone, so it knows better.
func TestStagedViewIsPerCloneNotPerUsername(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	t.Setenv("USER", "")
	t.Setenv("USERNAME", "dev-a")
	b := &Bridge{dir: root}

	const project = "infinity-core"
	// "laptop" was published by the same person, from somewhere else.
	writeManifest(t, root, project, manifestJSON(t,
		entry("laptop", "dev-a", "2026-08-19T15:41:24Z"),
		entry("desk", "dev-a", "2026-08-20T09:00:00Z"),
	))
	b.markPublished(project, []string{"desk"}) // only this clone's own publish

	staged := b.stagedView(project)
	if len(staged) != 1 || staged[0].ID != "desk" {
		t.Fatalf("staged view = %+v, want only this clone's chunk", staged)
	}
	// And it must still be considered present locally — publishing implies
	// having.
	if !b.imported()[project]["desk"] {
		t.Error("a published chunk was not recorded as present locally")
	}
}

// TestSilentContributorIsFlagged: a publisher whose chunks never carry a memory
// is the visible end of a failure that is silent on their side.
func TestSilentContributorIsFlagged(t *testing.T) {
	root := t.TempDir()
	b := &Bridge{dir: root}

	writeManifest(t, root, "infinity-core", manifestJSON(t,
		ChunkEntry{ID: "aaa", CreatedBy: "dev-a", CreatedAt: "2026-08-19T15:41:24Z", Memories: 278},
		ChunkEntry{ID: "bbb", CreatedBy: "dev-c", CreatedAt: "2026-08-20T09:05:11Z", Memories: 0},
	))

	got := onlyProject(t, b)
	if len(got.Silent) != 1 || got.Silent[0] != "dev-c" {
		t.Fatalf("silent contributors = %v, want [dev-c]", got.Silent)
	}

	// Someone who has landed memories and later publishes a relations-only
	// chunk is doing something perfectly normal.
	writeManifest(t, root, "infinity-core", manifestJSON(t,
		ChunkEntry{ID: "aaa", CreatedBy: "dev-a", CreatedAt: "2026-08-19T15:41:24Z", Memories: 278},
		ChunkEntry{ID: "ccc", CreatedBy: "dev-a", CreatedAt: "2026-08-21T09:00:00Z", Memories: 0},
	))
	if got := onlyProject(t, b); len(got.Silent) != 0 {
		t.Errorf("flagged %v for a relations-only publish, want nobody", got.Silent)
	}
}

// TestAdoptChunksGrowsTheManifest checks the other half of the staged publish:
// what the export produced in the temp directory has to land in the repo
// WITHOUT the staged manifest replacing the shared one, or publishing would
// erase every entry we did not stage — which is every teammate's.
func TestAdoptChunksGrowsTheManifest(t *testing.T) {
	root := t.TempDir()
	b := &Bridge{dir: root}

	const project = "infinity-core"
	staged := []ChunkEntry{entry("bbb", "dev-c", "2026-08-20T09:05:11Z")}
	writeManifest(t, root, project, manifestJSON(t,
		entry("aaa", "dev-a", "2026-08-19T15:41:24Z"),
		staged[0],
	))

	// What engram would leave behind in the temp dir: the staged chunk plus a
	// freshly written one.
	workDir := t.TempDir()
	writeManifest(t, workDir, ".", manifestJSON(t, staged[0], entry("ccc", "dev-c", "2026-08-20T10:00:00Z")))
	writeChunk(t, workDir, ".", "ccc", "obs-new")

	added, err := b.adoptChunks(workDir, project, staged)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if len(added) != 1 || added[0].ID != "ccc" {
		t.Fatalf("added = %+v, want just the new chunk ccc", added)
	}
	if _, err := os.Stat(filepath.Join(root, project, syncDirName, chunksDirName, "ccc.jsonl.gz")); err != nil {
		t.Errorf("new chunk file was not copied into the repo: %v", err)
	}

	m, err := readManifest(filepath.Join(root, project, syncDirName, manifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if got := strings.Join(ids(m.Chunks), ","); got != "aaa,bbb,ccc" {
		t.Errorf("manifest = %s, want aaa,bbb,ccc — the teammate's entry must survive", got)
	}
}

// writeChunk writes a gzipped chunk exactly as engram does: one JSON document
// holding an observations array, under .engram/chunks/<id>.jsonl.gz.
func writeChunk(t *testing.T, repo, project, id string, syncIDs ...string) {
	t.Helper()
	dir := filepath.Join(repo, project, syncDirName, chunksDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir chunks: %v", err)
	}
	obs := make([]map[string]string, 0, len(syncIDs))
	for _, s := range syncIDs {
		obs = append(obs, map[string]string{"sync_id": s, "title": "t"})
	}
	body, err := json.Marshal(map[string]any{"observations": obs})
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	f, err := os.Create(filepath.Join(dir, id+".jsonl.gz"))
	if err != nil {
		t.Fatalf("create chunk: %v", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	if _, err := gz.Write(body); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
}

func onlyProject(t *testing.T, b *Bridge) ProjectInfo {
	t.Helper()
	projects, err := b.Projects(context.Background())
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("got %d projects, want 1", len(projects))
	}
	return projects[0]
}

// ── helpers ───────────────────────────────────────────────────────────────

// A full re-export while behind reverts, for the whole team, every correction
// made while this clone was out of date: it sends our copy of every
// observation and the import takes the last writer without comparing dates.
//
// The refusal lives in publish, after the fast-forward, because that is the
// first moment the manifest is current — a check in the handler races with a
// teammate publishing between the page render and the click. So this asserts
// on publish itself, not on the counter it consults.
//
// The Bridge is built with a nil CLI runner on purpose: if the guard ever
// stops firing, the export runs and the test blows up instead of passing.
//
// Exercised through Push, the only publish button there is: a machine with no
// publishing history re-exports whole without anyone asking it to, which is
// exactly why the refusal cannot live behind a button the user chooses.
func TestFullReExportRefusedWhileBehind(t *testing.T) {
	ctx := context.Background()
	git := gitcli.New()
	if err := git.Available(ctx); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	t.Setenv("ENGRAM_DATA_DIR", filepath.Join(root, "data"))
	// Not the author of the chunk below, or notMissing would spare it as ours.
	t.Setenv("USER", "")
	t.Setenv("USERNAME", "dev-a")

	bare := filepath.Join(root, "origin.git")
	mustGit(t, "", "init", "--quiet", "--bare", bare)

	b := New(nil, git)
	if res := b.Setup(ctx, bare); res.Failed() {
		t.Fatalf("setup: %v\n%s", res.Err, res.Output)
	}

	const project = "infinity-core"
	writeManifest(t, b.dir, project, manifestJSON(t,
		entry("teammate1", "dev-c", "2026-08-20T13:11:40Z"),
	))

	res := b.Push(ctx, project)
	if !res.Blocked {
		t.Fatalf("a full re-export went ahead with an unapplied commit: %+v", res)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "not applied") {
		t.Errorf("refusal does not say why: %v", res.Err)
	}
	if res.Exported || res.Pushed {
		t.Error("refusal still exported or pushed something")
	}

	// Once the commit is applied the precondition is gone. Asserted on the
	// counter rather than by publishing, since publishing needs a real engram.
	b.markImported(project, []string{"teammate1"})
	if behind := b.UnappliedCommits(project); behind != 0 {
		t.Errorf("still %d behind after applying the only commit", behind)
	}
}

// Disconnect removes the clone, not the database — so it must not remove the
// record of what that database has applied and published.
//
// It used to: the ledger lived in <clone>/.git/. Every disconnect/reconnect
// then resurfaced the same teammate commits as unapplied and turned the next
// publish into a full re-export, and pulling only fixed it until the next
// disconnect. The information was never stale, it was just stored somewhere
// that got deleted with something else.
func TestLedgerSurvivesDisconnect(t *testing.T) {
	root := t.TempDir()
	// Mirror the real layout: the clone is a child of the data dir.
	b := &Bridge{dir: filepath.Join(root, "share-repo")}
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		t.Fatalf("mkdir clone: %v", err)
	}

	const project = "infinity-core"
	b.markImported(project, []string{"teammate1"})
	b.markPublished(project, []string{"mine1"})

	if err := b.Disconnect(); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if _, err := os.Stat(b.dir); !os.IsNotExist(err) {
		t.Fatal("disconnect left the clone behind")
	}

	// Reconnecting is a fresh clone at the same path; the ledger is not part
	// of it, so a brand-new Bridge must still see both buckets.
	fresh := &Bridge{dir: filepath.Join(root, "share-repo")}
	if !fresh.imported()[project]["teammate1"] {
		t.Error("forgot an applied commit across a disconnect: it would be offered for pull again")
	}
	if !fresh.publishedFrom(project)["mine1"] {
		t.Error("forgot what it published: the next publish would be a full re-export")
	}

	// And the panel must stop warning, which is what the user actually sees.
	writeManifest(t, fresh.dir, project, manifestJSON(t,
		entry("mine1", "dev-a", "2026-08-20T13:19:39Z"),
	))
	if fresh.FirstPublishHere(project) {
		t.Error("still claiming this machine never published, after a disconnect only")
	}
}

// The loop this closes: the panel says "no record of publishing", so you
// publish; the export runs with a zero watermark and engram produces nothing,
// because it never re-exports content whose hash it has already recorded; with
// no new chunk there is nothing to record; the warning comes back. Forever.
//
// An empty export from a zero watermark is engram stating this database has
// nothing left to export, so the repo's own chunks can be adopted as this
// machine's history — the one place where attributing by username is safe,
// because there is nothing left to truncate.
func TestEmptyExportAdoptsOwnChunksAndStopsTheLoop(t *testing.T) {
	root := t.TempDir()
	b := &Bridge{dir: filepath.Join(root, "share-repo")}
	t.Setenv("USER", "")
	t.Setenv("USERNAME", "dev-a")

	const project = "infinity-core"
	dirName := ProjectDirName(project)
	writeManifest(t, b.dir, dirName, manifestJSON(t,
		entry("mine1", "dev-a", "2026-08-19T15:41:24Z"),
		entry("theirs", "dev-c", "2026-08-20T13:11:40Z"),
		entry("mine2", "dev-a", "2026-08-20T13:19:39Z"),
	))

	// Starting state: history wiped, so the panel warns and the next export
	// stages nothing.
	if !b.FirstPublishHere(project) {
		t.Fatal("expected the warning before anything is recorded")
	}
	if v := b.stagedView(dirName); len(v) != 0 {
		t.Fatalf("expected a zero watermark, staged %+v", v)
	}

	// What publish does when the export comes back empty.
	b.markPublished(dirName, b.ownChunkIDs(dirName))

	if b.FirstPublishHere(project) {
		t.Error("warning survived an export that proved there is nothing to export")
	}
	staged := b.stagedView(dirName)
	if len(staged) != 2 {
		t.Fatalf("staged %d chunk(s), want our 2", len(staged))
	}
	// The safety half: a teammate's chunk must never be claimed as ours, or
	// their timestamp becomes our watermark and truncates the next export.
	for _, e := range staged {
		if e.CreatedBy != "dev-a" {
			t.Errorf("claimed a chunk published by %q", e.CreatedBy)
		}
	}
}

func entry(id, by, at string) ChunkEntry {
	return ChunkEntry{ID: id, CreatedBy: by, CreatedAt: at, Memories: 1}
}

func manifestJSON(t *testing.T, entries ...ChunkEntry) []byte {
	t.Helper()
	out, err := marshalManifest(Manifest{Version: 1, Chunks: entries})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return out
}

func writeManifest(t *testing.T, repo, project string, body []byte) {
	t.Helper()
	dir := filepath.Join(repo, project, syncDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWrite(t, filepath.Join(dir, manifestName), string(body))
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func branchOf(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(mustGit(t, dir, "branch", "--show-current"))
}

// The warning exists because the trap is silent: the Publish button promises
// "what you have written since your last publish" and, on a clone with no
// ledger, delivers the whole project. It must fire exactly when stagedView
// stages nothing AND the repo already holds chunks — a project nobody has
// published yet needs no warning, since a full export is the only export.
func TestFirstPublishHereWarnsOnlyWhenTheRepoIsNotEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	b := &Bridge{dir: root}

	const project = "infinity-core"
	if b.FirstPublishHere(project) {
		t.Error("warned about a project that is not in the repo at all")
	}

	writeManifest(t, root, project, manifestJSON(t))
	if b.FirstPublishHere(project) {
		t.Error("warned about a project with an empty manifest: a full export is the only export")
	}

	writeManifest(t, root, project, manifestJSON(t,
		entry("aaa", "dev-a", "2026-08-19T15:41:24Z"),
	))
	if !b.FirstPublishHere(project) {
		t.Error("a clone with no publishing history must be warned: its next publish is a Republish")
	}

	b.markPublished(project, []string{"aaa"})
	if b.FirstPublishHere(project) {
		t.Error("still warning after this clone published; the warning is once per clone")
	}
	if (&Bridge{dir: root}).FirstPublishHere(project) {
		t.Error("warning came back after a reload: the ledger did not persist")
	}
}

// The empty-export diagnosis must alarm only when something is actually lost.
// Chunks of ours already in the repo mean nothing is: the export found nothing
// to add because the database has not changed. Shouting "copy the old
// directory in" at that state sends someone repairing what is already fine.
func TestDiagnoseEmptyExportOnlyAlarmsWhenNothingOfOursIsPublished(t *testing.T) {
	root := t.TempDir()
	b := &Bridge{dir: filepath.Join(root, "share-repo")}
	t.Setenv("USER", "")
	t.Setenv("USERNAME", "dev-a")

	const dirName = "infinity-core"

	// Our work is in the repo: an empty export is just "nothing changed".
	writeManifest(t, b.dir, dirName, manifestJSON(t,
		entry("mine1", "dev-a", "2026-08-20T13:19:39Z"),
		entry("theirs", "dev-c", "2026-08-20T13:11:40Z"),
	))
	if w := b.diagnoseEmptyExport(dirName); w != "" {
		t.Errorf("warned while our work is present in the repository: %q", w)
	}

	// Nothing of ours in the repo and nothing exported: the real trap. By this
	// point publish has already escalated to a full re-export and that came
	// back empty too, so there is genuinely nothing left to try.
	writeManifest(t, b.dir, dirName, manifestJSON(t,
		entry("theirs", "dev-c", "2026-08-20T13:11:40Z"),
	))
	if w := b.diagnoseEmptyExport(dirName); w == "" {
		t.Error("stayed silent while none of our observations reached the repo")
	}
}

// The panel used to fast-forward on every render — ~2.7 s of network against a
// real remote — so navigating away and back paid for it twice to learn the
// same thing, and that window is what made a half-finished navigation
// reachable in the first place.
func TestRefreshIfStaleSkipsARecentFetch(t *testing.T) {
	ctx := context.Background()
	git := gitcli.New()
	if err := git.Available(ctx); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	t.Setenv("ENGRAM_DATA_DIR", filepath.Join(root, "data"))

	bare := filepath.Join(root, "origin.git")
	mustGit(t, "", "init", "--quiet", "--bare", bare)
	b := New(nil, git)
	if res := b.Setup(ctx, bare); res.Failed() {
		t.Fatalf("setup: %v\n%s", res.Err, res.Output)
	}
	// Give the clone a commit so Refresh does not short-circuit on HasCommits.
	mustWrite(t, filepath.Join(b.dir, "seed.txt"), "seed")
	mustGit(t, b.dir, "add", "seed.txt")
	mustGit(t, b.dir, "commit", "--quiet", "-m", "seed")
	mustGit(t, b.dir, "push", "--quiet", "--set-upstream", "origin", "HEAD")

	if err := b.RefreshIfStale(ctx); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	before := b.refreshedAt
	if before.IsZero() {
		t.Fatal("a successful refresh did not record when it happened")
	}

	if err := b.RefreshIfStale(ctx); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if !b.refreshedAt.Equal(before) {
		t.Error("fetched again inside the window: the timestamp moved")
	}

	// Outside the window it must go to the network again.
	b.mu.Lock()
	b.refreshedAt = time.Now().Add(-2 * refreshWindow)
	stale := b.refreshedAt
	b.mu.Unlock()
	if err := b.RefreshIfStale(ctx); err != nil {
		t.Fatalf("third refresh: %v", err)
	}
	if b.refreshedAt.Equal(stale) {
		t.Error("skipped the fetch even though the window had expired")
	}
}
