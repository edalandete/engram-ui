package gitbridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/edalandete/engram-ui/internal/gitcli"
)

// A project must publish to the repository the user chose, or to the one that
// can only be meant — never to a guess, since publishing exposes the project to
// everyone who can read that repository.
func TestReposRouteEachProjectToOneRepository(t *testing.T) {
	root := t.TempDir()
	r := &Repos{base: root, bridges: map[string]*Bridge{}}
	clone := func(id string, projects ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, id, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, p := range projects {
			writeManifest(t, filepath.Join(root, id), p, manifestJSON(t))
		}
	}
	idOf := func(b *Bridge) string {
		if b == nil {
			return ""
		}
		return b.ID()
	}

	clone("share-repo", "alpha", "both")
	if got := idOf(r.For("brand-new")); got != "share-repo" {
		t.Fatalf("one repository must take every project, got %q", got)
	}

	clone("share-repo-team", "beta", "both")
	for project, want := range map[string]string{
		"alpha":     "share-repo",      // only holder
		"Beta":      "share-repo-team", // case folds like the folder does
		"both":      "",                // ambiguous: ask
		"brand-new": "",                // nowhere yet: ask
	} {
		if got := idOf(r.For(project)); got != want {
			t.Errorf("For(%q) = %q, want %q", project, got, want)
		}
	}

	if err := r.Assign("both", "share-repo-team"); err != nil {
		t.Fatal(err)
	}
	if got := idOf(r.For("both")); got != "share-repo-team" {
		t.Errorf("explicit choice ignored: %q", got)
	}
	// Survives a restart: the choice is on disk, not in this struct.
	if got := idOf((&Repos{base: root, bridges: map[string]*Bridge{}}).For("both")); got != "share-repo-team" {
		t.Errorf("choice did not persist: %q", got)
	}

	for _, bad := range []string{"../share-repo", "share-repo-../x", "share-repo-missing", "engram.db", ""} {
		if r.Get(bad) != nil {
			t.Errorf("Get(%q) resolved to a repository", bad)
		}
		if r.Assign("alpha", bad) == nil {
			t.Errorf("Assign accepted %q", bad)
		}
	}

	// Removing a repository forgets its projects, or the next clone landing in
	// that directory would silently inherit them.
	if err := r.Remove("share-repo-team"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	left := r.readAssignments()
	r.mu.Unlock()
	if _, ok := left["both"]; ok {
		t.Error("mapping to a removed repository survived")
	}
	if got := len(r.List()); got != 1 {
		t.Errorf("%d repositories listed after removing one of two", got)
	}
}

func TestReposAddClonesBesideTheLegacyOne(t *testing.T) {
	ctx := context.Background()
	git := gitcli.New()
	if err := git.Available(ctx); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	t.Setenv("ENGRAM_DATA_DIR", filepath.Join(root, "data"))
	a, b := filepath.Join(root, "team-a.git"), filepath.Join(root, "team-b.git")
	mustGit(t, "", "init", "--quiet", "--bare", a)
	mustGit(t, "", "init", "--quiet", "--bare", b)

	r := NewRepos(nil, git)
	for _, url := range []string{a, b} {
		if res := r.Add(ctx, url); res.Failed() {
			t.Fatalf("add %s: %v\n%s", url, res.Err, res.Output)
		}
	}
	if res := r.Add(ctx, a); !res.Failed() {
		t.Error("connected the same remote twice")
	}
	got := r.List()
	if len(got) != 2 || got[0].ID() != "share-repo-team-a" || got[1].ID() != "share-repo-team-b" {
		t.Fatalf("listed %v", got)
	}
	// Siblings in the data dir, so every clone shares the one ledger.
	if got[0].importedPath() != got[1].importedPath() {
		t.Error("clones do not share the ledger")
	}
}

func TestFreeIDNamesTheCloneAfterItsRepository(t *testing.T) {
	root := t.TempDir()
	r := &Repos{base: root}
	for url, want := range map[string]string{
		"git@github.com:acme/Engram-Memories.git": "share-repo-engram-memories",
		"https://github.com/acme/team/":           "share-repo-team",
		`C:\tmp\origin.git`:                       "share-repo-origin",
	} {
		if got := r.freeID(url); got != want {
			t.Errorf("freeID(%q) = %q, want %q", url, got, want)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "share-repo-team", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := r.freeID("https://example.com/other/team.git"); got != "share-repo-team-2" {
		t.Errorf("same name, different remote: got %q, want share-repo-team-2", got)
	}
}
