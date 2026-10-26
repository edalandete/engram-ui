package cloudbridge

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edalandete/engram-ui/internal/cloudclient"
)

func sampleManifest() *cloudclient.Manifest {
	return &cloudclient.Manifest{Version: 1, Chunks: []cloudclient.ChunkEntry{
		{ID: "aaaa0001", CreatedBy: "alice", Memories: 5},
		{ID: "aaaa0002", CreatedBy: "alice", Memories: 3},
		{ID: "bbbb0001", CreatedBy: "bob", Memories: 2},
		{ID: "0000dddd", CreatedBy: "", Memories: 1},
	}}
}

func TestUsers(t *testing.T) {
	got := Users(sampleManifest())
	// sorted: "(unknown)", "alice", "bob"
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}
	// Users() reports chunk counts only; Obs is left 0 (manifest sums overcount,
	// so real obs counts come from UsersWithObsCounts which dedups payloads).
	if got[0].User != unknownUser || got[0].Chunks != 1 || got[0].Obs != 0 {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].User != "alice" || got[1].Chunks != 2 || got[1].Obs != 0 {
		t.Errorf("got[1] = %+v", got[1])
	}
	if got[2].User != "bob" || got[2].Chunks != 1 {
		t.Errorf("got[2] = %+v", got[2])
	}
}

func TestUsersNil(t *testing.T) {
	if Users(nil) != nil {
		t.Fatal("Users(nil) should be nil")
	}
}

func TestFilterChunks(t *testing.T) {
	m := sampleManifest()
	if all := FilterChunks(m.Chunks, ""); len(all) != 4 {
		t.Errorf("empty filter = %d, want 4 (all)", len(all))
	}
	if alice := FilterChunks(m.Chunks, "alice"); len(alice) != 2 {
		t.Errorf("alice = %d, want 2", len(alice))
	}
	if unk := FilterChunks(m.Chunks, unknownUser); len(unk) != 1 || unk[0].ID != "0000dddd" {
		t.Errorf("unknown filter = %+v, want [0000dddd]", unk)
	}
	if none := FilterChunks(m.Chunks, "carol"); len(none) != 0 {
		t.Errorf("carol = %d, want 0", len(none))
	}
}

func TestWriteSyncDirAndGzipRoundtrip(t *testing.T) {
	work := t.TempDir()
	entries := []cloudclient.ChunkEntry{
		{ID: "aaaa1111", CreatedBy: "alice", Memories: 5},
		{ID: "bbbb2222", CreatedBy: "alice", Memories: 3},
	}
	payloads := map[string][]byte{
		"aaaa1111": []byte(`{"observations":[{"sync_id":"o1"}]}`),
		"bbbb2222": []byte(`{"observations":[{"sync_id":"o2"}]}`),
	}
	fetch := func(id string) ([]byte, error) { return payloads[id], nil }

	if err := writeSyncDir(work, entries, fetch); err != nil {
		t.Fatal(err)
	}

	// manifest.json parses back to the filtered manifest
	mb, err := os.ReadFile(filepath.Join(work, ".engram", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m cloudclient.Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != 1 || len(m.Chunks) != 2 || m.Chunks[0].ID != "aaaa1111" {
		t.Fatalf("manifest mismatch: %+v", m)
	}

	// each chunk decompresses to the exact input (proves engram readGzip compat)
	for id, want := range payloads {
		p := filepath.Join(work, ".engram", "chunks", id+".jsonl.gz")
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(gz)
		gz.Close()
		f.Close()
		if !bytes.Equal(got, want) {
			t.Errorf("chunk %s roundtrip = %q, want %q", id, got, want)
		}
	}
}

func TestObservationSyncIDs(t *testing.T) {
	chunk := []byte(`{"sessions":[{"id":"s1"}],"observations":[{"sync_id":"obs-aaa","title":"x"},{"sync_id":"obs-bbb"},{"title":"no-sync-id"}],"prompts":[]}`)
	got := observationSyncIDs(chunk)
	if len(got) != 2 || got[0] != "obs-aaa" || got[1] != "obs-bbb" {
		t.Fatalf("got %v, want [obs-aaa obs-bbb]", got)
	}
	if ids := observationSyncIDs([]byte(`not json`)); ids != nil {
		t.Errorf("invalid json should yield nil, got %v", ids)
	}
	if ids := observationSyncIDs([]byte(`{"observations":[]}`)); len(ids) != 0 {
		t.Errorf("empty observations should yield empty, got %v", ids)
	}
}

// fakeCloud serves a manifest + chunk payloads over httptest, mirroring the
// engram cloud read endpoints (/sync/pull and /sync/pull/{id}).
func fakeCloud(t *testing.T, manifest cloudclient.Manifest, chunks map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/sync/pull"
		if r.URL.Path == prefix {
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, prefix+"/")
		body, ok := chunks[id]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
}

func obsPayload(syncIDs ...string) string {
	parts := make([]string, len(syncIDs))
	for i, id := range syncIDs {
		parts[i] = fmt.Sprintf(`{"sync_id":%q}`, id)
	}
	return `{"observations":[` + strings.Join(parts, ",") + `]}`
}

// TestUsersWithObsCountsFirstWins reproduces the molecular scenario: an earlier
// pusher (dev-s) uploads o1..o5, then a later reset+repush by dev-a
// re-uploads o3,o4,o5 plus new o6,o7. First-wins attribution credits the 3
// overlapping obs to dev-s (earlier), so counts are disjoint and sum to the
// 7 unique obs — NOT the inflated 5+5=10 of per-pusher dedup.
func TestUsersWithObsCountsFirstWins(t *testing.T) {
	manifest := cloudclient.Manifest{Version: 1, Chunks: []cloudclient.ChunkEntry{
		{ID: "cccc1111", CreatedBy: "dev-s", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "cccc2222", CreatedBy: "dev-a", CreatedAt: "2026-01-02T00:00:00Z"},
	}}
	chunks := map[string]string{
		"cccc1111": obsPayload("o1", "o2", "o3", "o4", "o5"),
		"cccc2222": obsPayload("o3", "o4", "o5", "o6", "o7"),
	}
	srv := fakeCloud(t, manifest, chunks)
	defer srv.Close()

	cc, err := cloudclient.New(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := New(nil).UsersWithObsCounts(context.Background(), cc, "molecular")
	if err != nil {
		t.Fatal(err)
	}

	by := map[string]UserCount{}
	total := 0
	for _, u := range got {
		by[u.User] = u
		total += u.Obs
	}
	if by["dev-s"].Obs != 5 {
		t.Errorf("dev-s obs = %d, want 5 (first to push o1..o5)", by["dev-s"].Obs)
	}
	if by["dev-a"].Obs != 2 {
		t.Errorf("dev-a obs = %d, want 2 (only o6,o7 are new; o3..o5 credited to dev-s)", by["dev-a"].Obs)
	}
	if total != 7 {
		t.Errorf("total obs = %d, want 7 unique (disjoint sum)", total)
	}
}

func TestWriteSyncDirFetchErrorAborts(t *testing.T) {
	work := t.TempDir()
	entries := []cloudclient.ChunkEntry{{ID: "abcd1234", CreatedBy: "a"}}
	fetch := func(id string) ([]byte, error) { return nil, io.ErrUnexpectedEOF }
	if err := writeSyncDir(work, entries, fetch); err == nil {
		t.Fatal("expected error when fetch fails")
	}
}
