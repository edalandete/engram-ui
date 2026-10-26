package client

import "testing"

// engram has no offset on /search, /observations/recent or /sessions/recent.
// window+skip are what make the Next button move; if they break, pagination
// silently serves page 1 forever — which is the bug they were written to kill.
func TestWindowAndSkip(t *testing.T) {
	if got := window(25, 0); got != 25 {
		t.Fatalf("window(25,0) = %d, want 25", got)
	}
	if got := window(25, 50); got != 75 {
		t.Fatalf("window(25,50) = %d, want 75", got)
	}
	if got := window(0, 50); got != 0 {
		t.Fatalf("window(0,50) = %d, want 0 (no limit means engram's default)", got)
	}

	rows := []int{0, 1, 2, 3, 4}
	if got := skip(rows, 0); len(got) != 5 {
		t.Fatalf("skip(_,0) dropped rows: %v", got)
	}
	if got := skip(rows, 2); len(got) != 3 || got[0] != 2 {
		t.Fatalf("skip(_,2) = %v, want [2 3 4]", got)
	}
	if got := skip(rows, 5); got != nil {
		t.Fatalf("skip past the end = %v, want nil", got)
	}
	if got := skip(rows, 99); got != nil {
		t.Fatalf("skip way past the end = %v, want nil", got)
	}
}
