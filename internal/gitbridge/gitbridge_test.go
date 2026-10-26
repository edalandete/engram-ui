package gitbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// The manifest is the only file two people can conflict on, and losing an entry
// from it permanently unshares those memories (engram will refuse to re-export
// a chunk it has already recorded). So the union is the one piece of logic here
// that must never drop a row.
func TestUnionManifestsKeepsBothSides(t *testing.T) {
	ours := []byte(`{"version":1,"chunks":[
		{"id":"aaa","created_by":"ana","created_at":"2026-01-01T10:00:00Z","memories":2},
		{"id":"bbb","created_by":"ana","created_at":"2026-01-02T10:00:00Z","memories":1}
	]}`)
	theirs := []byte(`{"version":1,"chunks":[
		{"id":"aaa","created_by":"ana","created_at":"2026-01-01T10:00:00Z","memories":2},
		{"id":"ccc","created_by":"luis","created_at":"2026-01-03T10:00:00Z","memories":5}
	]}`)

	merged, err := UnionManifests(ours, theirs)
	if err != nil {
		t.Fatalf("UnionManifests: %v", err)
	}
	m, err := parseManifest(merged)
	if err != nil {
		t.Fatalf("merged manifest does not parse: %v\n%s", err, merged)
	}

	got := ids(m.Chunks)
	want := []string{"aaa", "bbb", "ccc"} // deduplicated, chronological
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v", got, want)
	}
	if m.Version != 1 {
		t.Errorf("version = %d, want 1", m.Version)
	}
	// Payload fields must survive: they drive the contributor list.
	for _, c := range m.Chunks {
		if c.ID == "ccc" && (c.CreatedBy != "luis" || c.Memories != 5) {
			t.Errorf("entry ccc lost data: %+v", c)
		}
	}
}

// Both sides must reach the same bytes, or the next merge conflicts again.
func TestUnionManifestsIsOrderIndependent(t *testing.T) {
	a := []byte(`{"version":1,"chunks":[{"id":"zzz","created_at":"2026-01-05T00:00:00Z"}]}`)
	b := []byte(`{"version":1,"chunks":[{"id":"mmm","created_at":"2026-01-04T00:00:00Z"}]}`)

	ab, err := UnionManifests(a, b)
	if err != nil {
		t.Fatalf("UnionManifests(a,b): %v", err)
	}
	ba, err := UnionManifests(b, a)
	if err != nil {
		t.Fatalf("UnionManifests(b,a): %v", err)
	}
	if string(ab) != string(ba) {
		t.Errorf("merge is not commutative:\n a,b = %s\n b,a = %s", ab, ba)
	}
}

// Same timestamp is realistic (two exports in the same second) and must still
// produce one stable ordering everywhere.
func TestUnionManifestsBreaksTiesDeterministically(t *testing.T) {
	a := []byte(`{"version":1,"chunks":[{"id":"bbb","created_at":"2026-01-01T00:00:00Z"}]}`)
	b := []byte(`{"version":1,"chunks":[{"id":"aaa","created_at":"2026-01-01T00:00:00Z"}]}`)

	merged, err := UnionManifests(a, b)
	if err != nil {
		t.Fatalf("UnionManifests: %v", err)
	}
	m, _ := parseManifest(merged)
	if got := ids(m.Chunks); got[0] != "aaa" || got[1] != "bbb" {
		t.Errorf("ids = %v, want [aaa bbb]", got)
	}
}

// A manifest still holding conflict markers is not JSON. Refusing loudly keeps
// us from committing a corrupt index.
func TestUnionManifestsRejectsGarbage(t *testing.T) {
	valid := []byte(`{"version":1,"chunks":[]}`)
	broken := []byte("{\"version\":1,\"chunks\":[\n<<<<<<< HEAD\n]}")
	if _, err := UnionManifests(broken, valid); err == nil {
		t.Error("expected an error for a conflicted manifest, got nil")
	}
	if _, err := UnionManifests(valid, broken); err == nil {
		t.Error("expected an error for a conflicted manifest on the other side, got nil")
	}
}

// engram omits created_by when it cannot resolve a username; those chunks must
// stay selectable instead of matching every filter.
func TestFilterChunks(t *testing.T) {
	entries := []ChunkEntry{
		{ID: "a", CreatedBy: "ana"},
		{ID: "b", CreatedBy: "luis"},
		{ID: "c", CreatedBy: ""},
	}
	cases := []struct {
		filter string
		want   []string
	}{
		{"", []string{"a", "b", "c"}},
		{"ana", []string{"a"}},
		{unknownUser, []string{"c"}},
		{"nobody", nil},
	}
	for _, tc := range cases {
		got := ids(FilterChunks(entries, tc.filter))
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("FilterChunks(%q) = %v, want %v", tc.filter, got, tc.want)
		}
	}
}

// TestContributorCountsAreDisjoint pins the property that makes the panel's
// numbers trustworthy: a memory is credited to whoever published it FIRST, so
// the contributors add up to the project total instead of to the sum of the
// chunks. Chunks overlap heavily — a re-export republishes memories other
// chunks already carry — which is exactly how a project of 332 memories used to
// display as 896.
func TestContributorCountsAreDisjoint(t *testing.T) {
	root := t.TempDir()
	b := &Bridge{dir: root}

	const project = "infinity-core"
	writeManifest(t, root, project, manifestJSON(t,
		entry("a", "ana", "2026-01-01T00:00:00Z"),
		entry("b", "luis", "2026-01-02T00:00:00Z"),
		entry("c", "ana", "2026-01-03T00:00:00Z"),
	))
	writeChunk(t, root, project, "a", "obs-1", "obs-2")
	writeChunk(t, root, project, "b", "obs-3")
	writeChunk(t, root, project, "c", "obs-1", "obs-2", "obs-3", "obs-4") // ana re-exports everything

	m, err := readManifest(b.manifestPath(project))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	// Only ana's first chunk has been imported here.
	s := b.scanProject(project, m, map[string]bool{"a": true})

	if s.Memories != 4 {
		t.Errorf("distinct memories = %d, want 4 (the manifest counters would say 10)", s.Memories)
	}
	if s.New != 2 {
		t.Errorf("missing memories = %d, want 2 (obs-3 and obs-4)", s.New)
	}

	total := 0
	for _, u := range s.Users {
		total += u.Memories
	}
	if total != s.Memories {
		t.Errorf("contributors sum to %d, project has %d — the counts are not disjoint", total, s.Memories)
	}
	if len(s.Users) != 2 || s.Users[0].User != "ana" || s.Users[1].User != "luis" {
		t.Fatalf("contributors = %+v, want ana then luis", s.Users)
	}
	// ana published obs-1, obs-2 and obs-4 first; luis got obs-3 in before her
	// re-export, so it stays his.
	if s.Users[0].Memories != 3 || s.Users[0].New != 1 {
		t.Errorf("ana = %+v, want 3 memories with 1 to pull (obs-4)", s.Users[0])
	}
	if s.Users[1].Memories != 1 || s.Users[1].New != 1 {
		t.Errorf("luis = %+v, want 1 memory with 1 to pull (obs-3)", s.Users[1])
	}
}

// Project names come from repo names and can contain path separators. The
// directory must stay inside the clone and be stable across calls.
func TestProjectDirName(t *testing.T) {
	cases := map[string]string{
		"engram-ui":    "engram-ui",
		"acme/backend": "acme-backend",
		"../escape":    "escape",
		"my project":   "my-project",
		"  spaced  ":   "spaced",
		"":             "project",
		"///":          "project",
	}
	for in, want := range cases {
		if got := ProjectDirName(in); got != want {
			t.Errorf("ProjectDirName(%q) = %q, want %q", in, got, want)
		}
	}
	for in := range cases {
		got := ProjectDirName(in)
		if strings.ContainsAny(got, `/\`) || got == ".." {
			t.Errorf("ProjectDirName(%q) = %q escapes the repo directory", in, got)
		}
	}
}

// engram takes the project name from whatever directory or repo the user has
// checked out, so the same project reaches us in several spellings. They must
// land in ONE folder: git's index is case-sensitive but Windows and macOS
// filesystems are not, so two folders differing only in case cannot be checked
// out side by side and would break the shared repo for everyone on those
// platforms.
func TestProjectDirNameFoldsCase(t *testing.T) {
	spellings := []string{"infinity-core", "Infinity-core", "INFINITY-CORE", "Infinity-Core"}
	want := ProjectDirName(spellings[0])
	if want != "infinity-core" {
		t.Fatalf("baseline = %q, want infinity-core", want)
	}
	for _, s := range spellings {
		if got := ProjectDirName(s); got != want {
			t.Errorf("ProjectDirName(%q) = %q, want %q — would create a second folder", s, got, want)
		}
	}
}

// Directory names discovered in the clone are fed straight back into Pull and
// Contributors, so applying the mapping twice must not move the target.
func TestProjectDirNameIsIdempotent(t *testing.T) {
	for _, in := range []string{"Infinity-Core", "acme/Backend", "  My Project  ", "", "///", "engram-ui"} {
		once := ProjectDirName(in)
		if twice := ProjectDirName(once); twice != once {
			t.Errorf("ProjectDirName(%q) = %q, but reapplying gives %q", in, once, twice)
		}
	}
}

// Only manifests may be auto-resolved; anything else aborts the merge.
func TestIsManifestPath(t *testing.T) {
	yes := []string{".engram/manifest.json", "proj/.engram/manifest.json"}
	no := []string{"proj/.engram/chunks/abc.jsonl.gz", "manifest.json.bak", "README.md"}
	for _, p := range yes {
		if !isManifestPath(p) {
			t.Errorf("isManifestPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if isManifestPath(p) {
			t.Errorf("isManifestPath(%q) = true, want false", p)
		}
	}
}

// engram writes the manifest with a two-space indent and no trailing newline;
// matching that keeps our merge commits from churning a line every time.
func TestMarshalManifestMatchesEngramFormatting(t *testing.T) {
	out, err := marshalManifest(Manifest{Version: 1, Chunks: []ChunkEntry{{ID: "a"}}})
	if err != nil {
		t.Fatalf("marshalManifest: %v", err)
	}
	if strings.HasSuffix(string(out), "\n") {
		t.Error("manifest must not end with a newline")
	}
	if !strings.Contains(string(out), "\n  \"version\": 1") {
		t.Errorf("expected two-space indent, got:\n%s", out)
	}
	var round Manifest
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("output does not round-trip: %v", err)
	}
}

func ids(entries []ChunkEntry) []string {
	if len(entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

// The panel shows this output verbatim, so the CLI's closing "now go run git
// add" must not survive into it — publish() already committed and pushed.
func TestStripGitHint(t *testing.T) {
	const body = "Created chunk abc\n  Observations: 12\n"
	got := stripGitHint(body + "\nAdd to git:\n  git add .engram/ && git commit -m \"sync engram memories\"\n")
	if got != strings.TrimRight(body, "\n") {
		t.Fatalf("stripGitHint kept the tail: %q", got)
	}
	if got := stripGitHint(body); got != body {
		t.Fatalf("stripGitHint touched output without the hint: %q", got)
	}
	if got := stripGitHint(""); got != "" {
		t.Fatalf("stripGitHint(%q) = %q", "", got)
	}
}
