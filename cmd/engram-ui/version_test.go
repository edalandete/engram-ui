package main

import "testing"

// The version contract had two independent faults, and both are the kind that
// only bite the day someone finally raises MinEngram — at which point the UI
// refuses to start against every engram in existence and the message blames
// the wrong thing.
func TestCheckVersion(t *testing.T) {
	// MinEngram is v0.0.0 in this tree, so checkVersion short-circuits and
	// there is nothing to assert about comparisons through it. Test the piece
	// that does the work.
	t.Run("a git describe build counts as its tag", func(t *testing.T) {
		// semver reads everything after the dash as a pre-release, which sorts
		// BELOW the release. Untreated, a build eleven commits newer than
		// v1.16.3 compares as older than v1.16.3.
		got := releaseOf("v1.16.3-11-ga96efb3")
		if got != "v1.16.3" {
			t.Fatalf("releaseOf(git describe) = %q, want v1.16.3", got)
		}
		if releaseOf("v1.16.3") != "v1.16.3" {
			t.Errorf("a plain tag must survive unchanged, got %q", releaseOf("v1.16.3"))
		}
	})

	t.Run("no requirement declared means no check", func(t *testing.T) {
		// Including for the literal "0.1.0" that engram's /health hands back
		// regardless of build — the value that made the whole mechanism a trap.
		for _, v := range []string{"0.1.0", "", "not-a-version"} {
			if err := checkVersion(v); err != nil {
				t.Errorf("checkVersion(%q) = %v, want nil while MinEngram is v0.0.0", v, err)
			}
		}
	})
}
