package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// engram 2.0+ reads only the "current" project when none is named, and 3.0+
// refuses to update or delete an observation without its owner. Either
// regression empties the UI's all-projects screens or breaks edit/delete.
func TestProjectScopeAndOwner(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/observations/7":
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(Observation{ID: 7, Project: "owner"})
				return
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	ctx := context.Background()
	if _, err := c.Search(ctx, SearchQuery{Query: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Search(ctx, SearchQuery{Query: "x", Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteObservation(ctx, 7, true); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"GET /search?all_projects=true&q=x",
		"GET /search?project=p&q=x",
		"GET /observations/7",
		"DELETE /observations/7?expected_project=owner&hard=true",
	}
	if len(seen) != len(want) {
		t.Fatalf("requests = %q, want %q", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, seen[i], want[i])
		}
	}
}
