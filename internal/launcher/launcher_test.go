package launcher

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSiblingBinary(t *testing.T) {
	dir := t.TempDir()
	if got := siblingBinary(dir); got != "" {
		t.Fatalf("empty dir: got %q, want \"\"", got)
	}

	// A directory with the binary's name is not a binary.
	p := filepath.Join(dir, binName())
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := siblingBinary(dir); got != "" {
		t.Fatalf("dir named like the binary: got %q, want \"\"", got)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(p, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := siblingBinary(dir); got != p {
		t.Fatalf("got %q, want %q", got, p)
	}
}
