package gitbridge

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edalandete/engram-ui/internal/engramcli"
	"github.com/edalandete/engram-ui/internal/gitcli"
)

// TestMain lets this test binary stand in for `engram sync --project X`, so the
// publish path can be driven end to end without a real engram.
func TestMain(m *testing.M) {
	if src := os.Getenv("ENGRAM_UI_FAKE_EXPORT"); src != "" {
		os.Exit(fakeExport(src))
	}
	os.Exit(m.Run())
}

// fakeExport mimics engram's file-mode export in the working directory: the
// chunk document at src becomes ./.engram/chunks/<hash>.jsonl.gz, stamped with
// USER then USERNAME. Like engram, it never exports the same content twice: the
// hashes it has written are remembered per database (ENGRAM_DATA_DIR), not per
// manifest, which is what makes a discarded chunk impossible to re-export.
func fakeExport(src string) int {
	data, err := os.ReadFile(src)
	if err != nil {
		fmt.Println(err)
		return 1
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])[:8]

	seen := filepath.Join(os.Getenv("ENGRAM_DATA_DIR"), "fake-sync-chunks")
	prev, _ := os.ReadFile(seen)
	if strings.Contains(string(prev), id+"\n") {
		fmt.Println("All memories already exported")
		return 0
	}

	var doc struct {
		Observations []json.RawMessage `json:"observations"`
	}
	_ = json.Unmarshal(data, &doc)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(data)
	_ = gz.Close()
	if err := os.MkdirAll(filepath.Join(syncDirName, chunksDirName), 0o755); err != nil {
		fmt.Println(err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(syncDirName, chunksDirName, id+".jsonl.gz"), buf.Bytes(), 0o644); err != nil {
		fmt.Println(err)
		return 1
	}

	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	m, _ := readManifest(filepath.Join(syncDirName, manifestName))
	m.Version = 1
	m.Chunks = append(m.Chunks, ChunkEntry{ID: id, CreatedBy: user, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Memories: len(doc.Observations)})
	out, _ := marshalManifest(m)
	if err := os.WriteFile(filepath.Join(syncDirName, manifestName), out, 0o644); err != nil {
		fmt.Println(err)
		return 1
	}
	_ = os.MkdirAll(filepath.Dir(seen), 0o755)
	_ = os.WriteFile(seen, append(prev, []byte(id+"\n")...), 0o644)
	fmt.Println("Created chunk", id)
	return 0
}

const screenProject = "personal-proj"

// screenedRepo connects a Bridge, whose engram is this test binary, to an empty
// bare remote. The OS account is "os-user" throughout.
func screenedRepo(t *testing.T) (*Bridge, string) {
	t.Helper()
	git := gitcli.New()
	if err := git.Available(context.Background()); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	t.Setenv("ENGRAM_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("USER", "")
	t.Setenv("USERNAME", "os-user")

	bare := filepath.Join(root, "origin.git")
	mustGit(t, "", "init", "--quiet", "--bare", bare)
	b := New(engramcli.New(os.Args[0], ""), git)
	if res := b.Setup(context.Background(), bare); res.Failed() {
		t.Fatalf("setup: %v\n%s", res.Err, res.Output)
	}
	return b, b.RemoteURL(context.Background())
}

func writeRepoSettings(t *testing.T, url string, s RepoSettings) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"repos": map[string]RepoSettings{url: s}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("ENGRAM_DATA_DIR"), settingsFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, string(body))
}

func obs(syncID, project, scope, title, content string) map[string]any {
	return map[string]any{"sync_id": syncID, "project": project, "scope": scope, "title": title, "content": content}
}

// exportNext makes the fake engram's next export produce exactly these
// observations.
func exportNext(t *testing.T, observations ...map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"sessions": []any{}, "observations": observations, "prompts": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chunk.json")
	mustWrite(t, path, string(body))
	t.Setenv("ENGRAM_UI_FAKE_EXPORT", path)
}

func repoChunks(t *testing.T, b *Bridge) []ChunkEntry {
	t.Helper()
	m, err := readManifest(b.manifestPath(ProjectDirName(screenProject)))
	if err != nil {
		return nil
	}
	return m.Chunks
}

func remoteHasCommits(t *testing.T, bare string) bool {
	t.Helper()
	return strings.TrimSpace(mustGit(t, "", "ls-remote", "--heads", bare)) != ""
}

// A personal repository is published under a personal name, whatever the OS
// account is called.
func TestPublishSignsChunksWithTheRepositoryIdentity(t *testing.T) {
	b, url := screenedRepo(t)
	writeRepoSettings(t, url, RepoSettings{CreatedBy: "personal-user"})
	exportNext(t, obs("obs-1", screenProject, "project", "Clean", "nothing to hide"))

	res := b.Push(context.Background(), screenProject)
	if res.Err != nil || !res.Pushed {
		t.Fatalf("publish: %+v", res)
	}
	chunks := repoChunks(t, b)
	if len(chunks) != 1 || chunks[0].CreatedBy != "personal-user" {
		t.Fatalf("published chunks = %+v, want one signed personal-user", chunks)
	}
}

// A repository with no settings — every team repository today — behaves
// exactly as before: OS account, no screening.
func TestPublishWithoutSettingsIsUnchanged(t *testing.T) {
	b, _ := screenedRepo(t)
	exportNext(t, obs("obs-1", screenProject, "personal", "Mine", "acme-corp notes"))

	res := b.Push(context.Background(), screenProject)
	if res.Err != nil || !res.Pushed || res.Blocked {
		t.Fatalf("publish: %+v", res)
	}
	chunks := repoChunks(t, b)
	if len(chunks) != 1 || chunks[0].CreatedBy != "os-user" {
		t.Fatalf("published chunks = %+v, want one signed os-user", chunks)
	}
}

// The screening refuses a chunk that would leak something, says exactly what,
// and keeps the chunk aside: engram will never export that content again, so
// discarding it would leave "publish anyway" with nothing to publish.
func TestScreeningBlocksQuarantinesAndPublishesAnyway(t *testing.T) {
	b, url := screenedRepo(t)
	bare := url
	writeRepoSettings(t, url, RepoSettings{CreatedBy: "personal-user", Blocklist: []string{`acme-corp`}})
	exportNext(t,
		obs("obs-1", screenProject, "project", "Call notes", "Talked to ACME-Corp about the release"),
		obs("obs-2", screenProject, "personal", "Private", "just mine"),
		obs("obs-3", "other-proj", "project", "Elsewhere", "wrong project"),
		obs("obs-4", screenProject, "project", "Clean", "nothing to hide"),
	)
	ctx := context.Background()

	res := b.Push(ctx, screenProject)
	if !res.Blocked || res.Pushed || res.Exported {
		t.Fatalf("a chunk with findings went through: %+v", res)
	}
	if len(res.Findings) != 3 {
		t.Fatalf("findings = %+v, want obs-1, obs-2 and obs-3", res.Findings)
	}
	why := map[string]Finding{}
	for _, f := range res.Findings {
		why[f.ID] = f
	}
	if f := why["obs-1"]; !strings.Contains(f.Reason, "acme-corp") || !strings.Contains(f.Excerpt, "ACME-Corp") || f.Title != "Call notes" {
		t.Errorf("blocklist finding = %+v, want the pattern, the matched text and the title", f)
	}
	if f := why["obs-2"]; !strings.Contains(f.Reason, "personal") {
		t.Errorf("personal-scope finding = %+v", f)
	}
	if f := why["obs-3"]; !strings.Contains(f.Reason, "other-proj") {
		t.Errorf("wrong-project finding = %+v", f)
	}
	if got := repoChunks(t, b); len(got) != 0 {
		t.Errorf("a blocked chunk reached the repository: %+v", got)
	}
	if remoteHasCommits(t, bare) {
		t.Error("a blocked publish pushed something")
	}
	if !b.Quarantined(screenProject) {
		t.Fatal("the blocked chunk was not kept aside")
	}

	// Publishing again unchanged: engram has nothing new to export, so the
	// quarantined chunk is still the only way to publish that content.
	res = b.Push(ctx, screenProject)
	if res.Err != nil || res.Exported || res.Blocked {
		t.Fatalf("second publish: %+v", res)
	}
	if got := repoChunks(t, b); len(got) != 0 {
		t.Errorf("chunks reached the repository without screening: %+v", got)
	}
	if !b.Quarantined(screenProject) {
		t.Fatal("an empty export dropped the quarantined chunk")
	}

	res = b.PublishQuarantined(ctx, screenProject)
	if res.Err != nil || !res.Pushed {
		t.Fatalf("publish anyway: %+v", res)
	}
	chunks := repoChunks(t, b)
	if len(chunks) != 1 || chunks[0].CreatedBy != "personal-user" {
		t.Fatalf("published chunks = %+v, want the quarantined one", chunks)
	}
	if !b.publishedFrom(ProjectDirName(screenProject))[chunks[0].ID] {
		t.Error("the quarantined chunk is not recorded as published from here")
	}
	if b.Quarantined(screenProject) {
		t.Error("quarantine survived publishing it")
	}
	if !remoteHasCommits(t, bare) {
		t.Error("publish anyway did not push")
	}
}

// The commit author leaks as much as the chunk's created_by: engram-ui creates
// the clone, so without this it commits with the machine's global identity.
// The settings outlive the clone, so they carry the identity, on both paths
// that commit.
func TestPublishCommitsWithTheRepositoryGitIdentity(t *testing.T) {
	b, url := screenedRepo(t)
	// The environment outranks git config; clear what screenedRepo set.
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	writeRepoSettings(t, url, RepoSettings{
		CreatedBy: "personal-user", Blocklist: []string{`acme-corp`},
		GitName: "Personal Name", GitEmail: "personal@example.invalid",
	})
	ctx := context.Background()
	const want = "Personal Name <personal@example.invalid>|Personal Name <personal@example.invalid>"
	author := func() string {
		return strings.TrimSpace(mustGit(t, b.Dir(), "log", "-1", "--format=%an <%ae>|%cn <%ce>"))
	}

	exportNext(t, obs("obs-1", screenProject, "project", "Call notes", "Talked to acme-corp"))
	if res := b.Push(ctx, screenProject); !res.Blocked {
		t.Fatalf("precondition: %+v", res)
	}
	if res := b.PublishQuarantined(ctx, screenProject); res.Err != nil || !res.Pushed {
		t.Fatalf("publish anyway: %+v", res)
	}
	if got := author(); got != want {
		t.Errorf("publish anyway committed as %q, want %q", got, want)
	}

	exportNext(t, obs("obs-2", screenProject, "project", "Clean", "nothing to hide"))
	if res := b.Push(ctx, screenProject); res.Err != nil || !res.Pushed {
		t.Fatalf("publish: %+v", res)
	}
	if got := author(); got != want {
		t.Errorf("publish committed as %q, want %q", got, want)
	}
}

// Fixing the observations and publishing again produces a new chunk that
// covers the same ground, so the quarantined one is obsolete.
func TestCleanPublishSupersedesQuarantine(t *testing.T) {
	b, url := screenedRepo(t)
	writeRepoSettings(t, url, RepoSettings{CreatedBy: "personal-user", Blocklist: []string{`acme-corp`}})
	ctx := context.Background()

	exportNext(t, obs("obs-1", screenProject, "project", "Call notes", "Talked to acme-corp"))
	if res := b.Push(ctx, screenProject); !res.Blocked {
		t.Fatalf("precondition: %+v", res)
	}

	exportNext(t, obs("obs-1", screenProject, "project", "Call notes", "Talked to a customer"))
	res := b.Push(ctx, screenProject)
	if res.Err != nil || !res.Pushed {
		t.Fatalf("clean publish: %+v", res)
	}
	if b.Quarantined(screenProject) {
		t.Error("a superseded chunk stayed in quarantine")
	}
	if got := b.PublishQuarantined(ctx, screenProject); got.Err == nil {
		t.Errorf("publish anyway with nothing quarantined = %+v, want an error", got)
	}
}

// Settings that cannot be read must stop a publish: silently falling back would
// publish unscreened and under the OS account.
func TestBrokenSettingsRefusePublish(t *testing.T) {
	cases := map[string]func(t *testing.T, url string){
		"unreadable file": func(t *testing.T, _ string) {
			mustWrite(t, filepath.Join(os.Getenv("ENGRAM_DATA_DIR"), settingsFileName), "{not json")
		},
		"invalid pattern": func(t *testing.T, url string) {
			writeRepoSettings(t, url, RepoSettings{Blocklist: []string{`(`}})
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			b, url := screenedRepo(t)
			if err := os.MkdirAll(os.Getenv("ENGRAM_DATA_DIR"), 0o755); err != nil {
				t.Fatal(err)
			}
			breakIt(t, url)
			exportNext(t, obs("obs-1", screenProject, "project", "Clean", "nothing to hide"))

			res := b.Push(context.Background(), screenProject)
			if res.Err == nil || res.Pushed {
				t.Fatalf("published with broken settings: %+v", res)
			}
			if got := repoChunks(t, b); len(got) != 0 {
				t.Errorf("chunks reached the repository: %+v", got)
			}
		})
	}
}

// "Which chunks are mine?" must use the name the chunks are signed with.
// Asking with the OS account would find none of them and turn every publish
// into a full re-export, and show your own commits as waiting to be pulled.
func TestRepositoryIdentityOwnsItsChunks(t *testing.T) {
	b, url := screenedRepo(t)
	dirName := ProjectDirName(screenProject)
	writeManifest(t, b.Dir(), dirName, manifestJSON(t, entry("c1", "personal-user", "2026-10-26T10:00:00Z")))

	if got := b.ownChunkIDs(dirName); len(got) != 0 {
		t.Fatalf("without settings the OS account owns %v", got)
	}
	if n := b.UnappliedCommits(screenProject); n != 1 {
		t.Fatalf("without settings: %d unapplied, want 1", n)
	}

	writeRepoSettings(t, url, RepoSettings{CreatedBy: "personal-user"})
	if got := b.ownChunkIDs(dirName); len(got) != 1 || got[0] != "c1" {
		t.Errorf("own chunks = %v, want [c1]", got)
	}
	if n := b.UnappliedCommits(screenProject); n != 0 {
		t.Errorf("our own chunk counts as %d unapplied commit(s)", n)
	}
}
