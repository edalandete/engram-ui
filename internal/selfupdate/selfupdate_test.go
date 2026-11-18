package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		tag, current string
		want         bool
	}{
		{"v0.1.24", "v0.1.23", true},
		{"v0.1.24", "v0.1.9", true}, // numeric, not text order
		{"v0.1.24", "v0.1.24", false},
		{"v0.1.24", "v0.1.30", false},
		{"v0.1.24", "dev", true}, // go install / go build without VERSION
	} {
		if got := Newer(c.tag, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.tag, c.current, got, c.want)
		}
	}
}

// The one step that could leave no engram-ui at all.
func TestReplaceKeepsThePreviousExe(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "engram-ui.exe")
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	write(exe, "v1")
	write(exe+".old", "v0") // left by the update before
	write(exe+".new", "v2")

	if err := replace(exe, exe+".new"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := read(exe); got != "v2" {
		t.Errorf("exe = %q, want the new one", got)
	}
	if got := read(exe + ".old"); got != "v1" {
		t.Errorf(".old = %q, want the previous exe", got)
	}

	// No .new to install: the previous exe must end up back in place.
	if err := replace(exe, exe+".new"); err == nil {
		t.Fatal("replace without a .new succeeded")
	}
	if got := read(exe); got != "v2" {
		t.Errorf("exe after a failed replace = %q, want v2 back", got)
	}
}

// Program Files: no right to write must ask for administrator, not reach gh.
func TestInstallWithoutWriteRightNeedsAdmin(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder this user cannot write")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	err := Install(context.Background(), "v0.1.24", filepath.Join(dir, "engram-ui.exe"))
	if !errors.Is(err, ErrNeedsAdmin) {
		t.Errorf("Install = %v, want ErrNeedsAdmin", err)
	}
}
