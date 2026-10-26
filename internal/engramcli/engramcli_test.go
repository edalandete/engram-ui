package engramcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain lets this test binary stand in for engram: with
// ENGRAM_UI_FAKE_ENGRAM set it prints the identity engram would stamp on a
// chunk (USER, then USERNAME) and exits before any test runs.
func TestMain(m *testing.M) {
	if os.Getenv("ENGRAM_UI_FAKE_ENGRAM") == "1" {
		fmt.Printf("USER=%s USERNAME=%s\n", os.Getenv("USER"), os.Getenv("USERNAME"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A repository can be published under a name other than the OS account, so
// the export must hand engram that name in the only place it reads one.
func TestLocalExportStampsCreatedBy(t *testing.T) {
	t.Setenv("ENGRAM_UI_FAKE_ENGRAM", "1")
	t.Setenv("USER", "")
	t.Setenv("USERNAME", "os-user")
	rn := New(os.Args[0], "")
	ctx := context.Background()

	res := rn.LocalExport(ctx, t.TempDir(), "proj", "personal-user")
	if res.Failed() {
		t.Fatalf("export: %v\n%s", res.Err, res.Output)
	}
	if !strings.Contains(res.Output, "USER=personal-user USERNAME=personal-user") {
		t.Errorf("export did not run as the given identity: %q", res.Output)
	}

	res = rn.LocalExport(ctx, t.TempDir(), "proj", "")
	if res.Failed() {
		t.Fatalf("export: %v\n%s", res.Err, res.Output)
	}
	if !strings.Contains(res.Output, "USERNAME=os-user") {
		t.Errorf("an empty identity must leave the OS account alone: %q", res.Output)
	}
}

func TestStripUpdateCheckNoise(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "dev build preamble before enroll output",
			in:   "Could not check for updates: development builds do not map to a release version.\n\n✓ Project \"infinity-core\" enrolled for cloud sync\n",
			want: "✓ Project \"infinity-core\" enrolled for cloud sync\n",
		},
		{
			name: "github rate limit preamble before enroll output",
			in:   "Could not check for updates: GitHub API returned 403 Forbidden. Set GH_TOKEN or GITHUB_TOKEN to reduce rate limits.\n\n✓ Project \"infinity-core\" enrolled for cloud sync\n",
			want: "✓ Project \"infinity-core\" enrolled for cloud sync\n",
		},
		{
			name: "update available multi-line preamble",
			in:   "Update available: 1.16.0 -> 1.16.1\nTo update:\n  brew upgrade engram\n\n✓ done\n",
			want: "✓ done\n",
		},
		{
			name: "leading blank lines before preamble",
			in:   "\n\nCould not check for updates: current version is unknown.\n\nreal output\n",
			want: "real output\n",
		},
		{
			name: "no preamble passes through unchanged",
			in:   "✓ Project \"infinity-core\" enrolled for cloud sync\n",
			want: "✓ Project \"infinity-core\" enrolled for cloud sync\n",
		},
		{
			name: "preamble with no trailing output is fully stripped",
			in:   "Could not check for updates: development builds do not map to a release version.\n\n",
			want: "",
		},
		{
			name: "preamble headline with no blank separator is fully stripped",
			in:   "Could not check for updates: development builds do not map to a release version.",
			want: "",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripUpdateCheckNoise(tt.in); got != tt.want {
				t.Errorf("stripUpdateCheckNoise(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestClearServerKeepsTheToken is the reason this writes JSON through a map
// instead of a struct: unlinking the server must not silently destroy the
// credential sitting in the same file, nor any key a future engram adds.
func TestClearServerKeepsTheToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENGRAM_DATA_DIR", dir)
	t.Setenv("ENGRAM_CLOUD_SERVER", "")

	path := filepath.Join(dir, "cloud.json")
	body := `{"server_url":"http://10.0.0.1:20001","token":"secret","future_key":42}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CurrentServer(); got != "http://10.0.0.1:20001" {
		t.Fatalf("precondition: CurrentServer() = %q", got)
	}

	if err := ClearServer(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := CurrentServer(); got != "" {
		t.Errorf("CurrentServer() = %q after clearing, want empty", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cc map[string]any
	if err := json.Unmarshal(raw, &cc); err != nil {
		t.Fatalf("rewritten config is not valid JSON: %v", err)
	}
	if cc["token"] != "secret" {
		t.Errorf("token = %v, want it preserved", cc["token"])
	}
	if cc["future_key"] == nil {
		t.Error("an unknown key was dropped; the rewrite must preserve the whole file")
	}
	if _, still := cc["server_url"]; still {
		t.Error("server_url survived the clear")
	}
}

// TestClearServerRefusesWhenEnvWins: clearing the file would look like it
// worked while the environment kept overriding it.
func TestClearServerRefusesWhenEnvWins(t *testing.T) {
	t.Setenv("ENGRAM_DATA_DIR", t.TempDir())
	t.Setenv("ENGRAM_CLOUD_SERVER", "http://from-env:1234")
	if err := ClearServer(); err == nil {
		t.Fatal("expected ClearServer to refuse while ENGRAM_CLOUD_SERVER is set")
	}
}

// TestClearServerWithoutConfigIsFine: nothing configured, nothing to do.
func TestClearServerWithoutConfigIsFine(t *testing.T) {
	t.Setenv("ENGRAM_DATA_DIR", t.TempDir())
	t.Setenv("ENGRAM_CLOUD_SERVER", "")
	if err := ClearServer(); err != nil {
		t.Errorf("clear with no config = %v, want nil", err)
	}
}
