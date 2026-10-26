package cloudclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A chunk id becomes a filename on this machine and a path segment in a URL,
// and it arrives inside JSON from the cloud server. filepath.Join CLEANS a path
// but does not CONFINE it: joining a temp dir with "../../../../evil" resolves
// cleanly OUT of that directory and truncates whatever is there. So the id is
// checked at the boundary instead of the path being checked afterwards.
func TestManifestRejectsHostileChunkIDs(t *testing.T) {
	for _, id := range []string{
		"../../../../../../evil",
		"..",
		"a/b",
		`a\b`,
		"aaaa111",   // too short
		"aaaa11111", // too long
		"AAAA1111",  // engram emits lowercase hex only
		"zzzz1111",  // not hex
		"",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(Manifest{Version: 1, Chunks: []ChunkEntry{{ID: id}}})
		}))
		c, err := New(srv.URL, "")
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		_, err = c.ReadManifest(context.Background(), "P")
		srv.Close()
		if err == nil {
			t.Errorf("manifest with id %q was accepted", id)
		}
	}
}

// ReadChunk is exported and takes the id directly, so it cannot lean on
// Manifest having screened it first.
func TestReadChunkRejectsHostileChunkID(t *testing.T) {
	c, err := New("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// No server on that port: reaching the network at all would already mean
	// the id got through.
	_, err = c.ReadChunk(context.Background(), "P", "../../../etc/passwd")
	if err == nil {
		t.Fatal("hostile chunk id accepted")
	}
	if !strings.Contains(err.Error(), "invalid chunk id") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"empty", "", true},
		{"no scheme", "127.0.0.1:18080", true},
		{"ftp scheme", "ftp://host", true},
		{"http ok", "http://127.0.0.1:18080", false},
		{"https ok", "https://cloud.example.com", false},
		{"trailing slash trimmed", "http://host:18080/", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.url, "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("New(%q) err=%v, wantErr=%v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestReadManifest(t *testing.T) {
	want := Manifest{Version: 1, Chunks: []ChunkEntry{
		{ID: "aaaa1111", CreatedBy: "alice", CreatedAt: "2026-01-01T00:00:00Z", Sessions: 1, Memories: 5, Prompts: 2},
		{ID: "bbbb2222", CreatedBy: "bob", Memories: 3},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/pull" {
			t.Errorf("path = %q, want /sync/pull", r.URL.Path)
		}
		if got := r.URL.Query().Get("project"); got != "proj x" {
			t.Errorf("project query = %q, want %q", got, "proj x")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("auth = %q, want Bearer tok123", got)
		}
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok123")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadManifest(context.Background(), "proj x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || len(got.Chunks) != 2 || got.Chunks[0].CreatedBy != "alice" || got.Chunks[1].ID != "bbbb2222" {
		t.Fatalf("manifest mismatch: %+v", got)
	}
}

func TestReadChunk(t *testing.T) {
	body := []byte(`{"observations":[{"sync_id":"o1"}]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/pull/cccc3333" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("project"); got != "P" {
			t.Errorf("project = %q", got)
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "")
	got, err := c.ReadChunk(context.Background(), "P", "cccc3333")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("chunk body = %q, want %q", got, body)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrForbidden},
	}
	for _, tt := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.code)
		}))
		c, _ := New(srv.URL, "")
		_, err := c.ReadManifest(context.Background(), "P")
		if !errors.Is(err, tt.want) {
			t.Errorf("status %d → err %v, want %v", tt.code, err, tt.want)
		}
		srv.Close()
	}
}

func TestReadChunkNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "")
	_, err := c.ReadChunk(context.Background(), "P", "deadbeef")
	if !errors.Is(err, ErrChunkNotFound) {
		t.Fatalf("err = %v, want ErrChunkNotFound", err)
	}
}

func TestReadChunkEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // empty body
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "")
	_, err := c.ReadChunk(context.Background(), "P", "x")
	if err == nil {
		t.Fatal("expected error on empty body")
	}
}
