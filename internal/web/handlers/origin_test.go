package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The UI listens on a fixed loopback port with no authentication, so the only
// thing standing between a random web page and POST /observations/bulk is this
// check. The attack needs no script: a plain <form action="http://127.0.0.1:7438/...">
// that auto-submits, on any site the user happens to visit.
func TestSameOriginGuard(t *testing.T) {
	reached := false
	guarded := sameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	call := func(method string, header map[string]string) int {
		reached = false
		r := httptest.NewRequest(method, "http://127.0.0.1:7438/observations/bulk", nil)
		r.Host = "127.0.0.1:7438"
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		guarded.ServeHTTP(w, r)
		if got := w.Result().StatusCode; got == http.StatusForbidden && reached {
			t.Fatal("handler ran despite being refused")
		}
		return w.Result().StatusCode
	}

	t.Run("our own forms pass", func(t *testing.T) {
		if got := call("POST", map[string]string{"Sec-Fetch-Site": "same-origin"}); got != 200 {
			t.Errorf("same-origin POST = %d, want 200", got)
		}
	})

	// The whole point. "same-site" is refused too: a subdomain is not us, and
	// on loopback there is no such thing as a sibling site we trust.
	for _, site := range []string{"cross-site", "same-site", "none"} {
		t.Run("refused: "+site, func(t *testing.T) {
			if got := call("POST", map[string]string{"Sec-Fetch-Site": site}); got != http.StatusForbidden {
				t.Errorf("%s POST = %d, want 403", site, got)
			}
		})
	}

	// Browsers that predate Fetch Metadata still send Origin on form posts.
	t.Run("falls back to Origin", func(t *testing.T) {
		if got := call("POST", map[string]string{"Origin": "http://127.0.0.1:7438"}); got != 200 {
			t.Errorf("matching Origin = %d, want 200", got)
		}
		if got := call("POST", map[string]string{"Origin": "http://evil.example"}); got != http.StatusForbidden {
			t.Errorf("foreign Origin = %d, want 403", got)
		}
	})

	// curl and scripts send neither header. CSRF is a browser problem, and
	// anyone running code on this machine has better options than a form.
	t.Run("non-browser callers pass", func(t *testing.T) {
		if got := call("POST", nil); got != 200 {
			t.Errorf("headerless POST = %d, want 200", got)
		}
	})

	// Navigating by typed address or bookmark arrives as Sec-Fetch-Site: none.
	// Blocking that would break every page in the app.
	t.Run("GET is never blocked", func(t *testing.T) {
		if got := call("GET", map[string]string{"Sec-Fetch-Site": "none"}); got != 200 {
			t.Errorf("GET with site=none = %d, want 200", got)
		}
		if got := call("GET", map[string]string{"Sec-Fetch-Site": "cross-site"}); got != 200 {
			t.Errorf("GET cross-site = %d, want 200", got)
		}
	})
}

// ?to= is a query parameter and Referer is a request header, so both are
// attacker-controlled. An unchecked redirect makes this UI a laundering hop
// for a phishing link: the victim sees an address on their own machine and
// lands somewhere else.
func TestLocalRedirect(t *testing.T) {
	const host = "127.0.0.1:7438"

	for _, ok := range []struct{ in, want string }{
		{"/observations", "/observations"},
		{"/sessions?limit=10", "/sessions?limit=10"},
		{"http://127.0.0.1:7438/topics", "/topics"},
		{"http://127.0.0.1:7438/observations?q=x", "/observations?q=x"},
	} {
		if got := localRedirect(ok.in, host); got != ok.want {
			t.Errorf("localRedirect(%q) = %q, want %q", ok.in, got, ok.want)
		}
	}

	for _, bad := range []string{
		"https://evil.example/phish",
		"http://127.0.0.1:9999/other-app",
		"//evil.example", // protocol-relative: leaves the site despite the slash
		`/\evil.example`, // some browsers read this as protocol-relative too
		"javascript:alert(1)",
		"observations", // relative, would resolve against the current path
		"",
	} {
		if got := localRedirect(bad, host); got != "" {
			t.Errorf("localRedirect(%q) = %q, want it refused", bad, got)
		}
	}
}
