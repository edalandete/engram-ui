package handlers

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edalandete/engram-ui/internal/client"
	"github.com/edalandete/engram-ui/internal/gitbridge"
	"github.com/edalandete/engram-ui/internal/web"
)

// A blocked publish has to say which observations and why, or the user can
// only guess what to fix.
func TestPushResultListsScreeningFindings(t *testing.T) {
	out := pushResultFrom(gitbridge.PushResult{
		Project: "proj",
		Blocked: true,
		Err:     errors.New("1 observation(s) failed the screening; nothing was published"),
		Findings: []gitbridge.Finding{
			{ID: "obs-1", Title: "Call notes", Reason: "matches acme-corp", Excerpt: "…Talked to ACME-Corp about…"},
		},
	})
	if !out.Blocked {
		t.Fatal("a screening refusal must render as blocked, not failed")
	}
	for _, want := range []string{"Call notes", "obs-1", "matches acme-corp", "ACME-Corp"} {
		if !strings.Contains(out.Output, want) {
			t.Errorf("banner output %q is missing %q", out.Output, want)
		}
	}
}

func renderSharePage(t *testing.T, data map[string]any) string {
	t.Helper()
	tpl, err := New(nil, nil, nil, nil, "").tpl.Clone()
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if _, err := tpl.ParseFS(web.TemplatesFS, "templates/share.html.tmpl"); err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return buf.String()
}

// The findings show on the blocked banner, and "publish anyway" is offered
// for as long as a chunk is quarantined — not only on the banner, which is gone
// after the next navigation.
func TestSharePageOffersPublishAnywayOnlyWhenQuarantined(t *testing.T) {
	data := func(quarantined bool) map[string]any {
		return map[string]any{
			"Title": "Engram Shared", "Theme": "dark", "CurrentProject": "proj",
			"Projects": []string{"proj"}, "Path": "/share", "SyncProject": "proj",
			"PendingConflicts": 0, "UnappliedCommits": 0, "ProjectRepo": "share-repo",
			"Repos":       []shareRepo{{ID: "share-repo", URL: "git@example.invalid:me/memories.git"}},
			"Quarantined": quarantined,
			"Result":      &syncResult{Action: "share-push", Blocked: true, Message: "blocked", Output: "obs-1 · Call notes — matches acme-corp"},
		}
	}

	out := renderSharePage(t, data(true))
	if !strings.Contains(out, "matches acme-corp") {
		t.Error("the blocked banner hides the findings")
	}
	if !strings.Contains(out, `action="/share/publish-anyway"`) {
		t.Error("no way to publish a quarantined chunk")
	}

	if out := renderSharePage(t, data(false)); strings.Contains(out, `action="/share/publish-anyway"`) {
		t.Error("offered to publish anyway with nothing quarantined")
	}
}

func TestPublishAnywayRouteExists(t *testing.T) {
	// Any page, the error page included, asks engram for header stats.
	engram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer engram.Close()

	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7438/share/publish-anyway", nil)
	r.Host = "127.0.0.1:7438"
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	New(client.New(engram.URL, ""), nil, nil, nil, "").Routes().ServeHTTP(w, r)
	if code := w.Result().StatusCode; code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
		t.Errorf("POST /share/publish-anyway = %d, want a handler", code)
	}
}
