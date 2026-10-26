package gitcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `git clone <url> <dir>` without a `--` lets a leading dash become a git
// option, and `--upload-pack=<command>` makes git run that command. The URL
// here arrives from a form field on a server with no authentication, so this
// is remote code execution one missing token away.
//
// Run against the real git rather than asserting on argv: the vulnerability is
// git's parsing, not ours, so a test that only checks our arguments would keep
// passing if git ever changed how it reads them.
func TestCloneCannotBeTalkedIntoRunningACommand(t *testing.T) {
	ctx := context.Background()
	rn := New()
	if err := rn.Available(ctx); err != nil {
		t.Skip("git not available")
	}

	// Clone runs with the process working directory, which is where the
	// command would drop its file. Move somewhere disposable and watch it.
	dir := t.TempDir()
	t.Chdir(dir)

	marker := "PWNED.txt"
	res := rn.Clone(ctx, "--upload-pack=touch "+marker, filepath.Join(dir, "dest"))

	if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
		t.Fatalf("git executed the injected command; the `--` separator is missing from Clone.\ngit said: %s", res.Output)
	}
	if !res.Failed() {
		t.Errorf("a dash-prefixed URL should fail as a missing repository, got success: %s", res.Output)
	}
}

// The separator must not break the ordinary case it protects.
func TestCloneStillClonesARealRepository(t *testing.T) {
	ctx := context.Background()
	rn := New()
	if err := rn.Available(ctx); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")

	origin := filepath.Join(root, "origin.git")
	if res := rn.run(ctx, "", "init", "--quiet", "--bare", origin); res.Failed() {
		t.Fatalf("init bare: %v\n%s", res.Err, res.Output)
	}

	dest := filepath.Join(root, "clone")
	if res := rn.Clone(ctx, origin, dest); res.Failed() {
		t.Fatalf("clone of a real repository failed: %v\n%s", res.Err, res.Output)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Errorf("clone produced no working tree: %v", err)
	}
}

// A publish that commits and then fails to push leaves the work on this
// machine only. The export will never produce it again — it is already
// committed — so the next publish finds a clean tree. Without this signal it
// reports "nothing new to publish" and the observations stay here forever
// while the panel looks healthy.
func TestHasUnpushedCommits(t *testing.T) {
	ctx := context.Background()
	rn := New()
	if err := rn.Available(ctx); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")

	origin := filepath.Join(root, "origin.git")
	if res := rn.run(ctx, "", "init", "--quiet", "--bare", origin); res.Failed() {
		t.Fatalf("init bare: %v\n%s", res.Err, res.Output)
	}
	clone := filepath.Join(root, "clone")
	if res := rn.Clone(ctx, origin, clone); res.Failed() {
		t.Fatalf("clone: %v\n%s", res.Err, res.Output)
	}

	if rn.HasUnpushedCommits(ctx, clone) {
		t.Error("a fresh empty clone has nothing unpushed")
	}

	if err := os.WriteFile(filepath.Join(clone, "note.txt"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := rn.Add(ctx, clone, "note.txt"); res.Failed() {
		t.Fatalf("add: %v\n%s", res.Err, res.Output)
	}
	if res := rn.Commit(ctx, clone, "committed but not pushed"); res.Failed() {
		t.Fatalf("commit: %v\n%s", res.Err, res.Output)
	}

	// The state a failed push leaves behind: committed, no upstream reached.
	if !rn.HasUnpushedCommits(ctx, clone) {
		t.Fatal("a local commit that never reached the remote must read as unpushed")
	}

	if res := rn.Push(ctx, clone); res.Failed() {
		t.Fatalf("push: %v\n%s", res.Err, res.Output)
	}
	if rn.HasUnpushedCommits(ctx, clone) {
		t.Error("still unpushed after a successful push")
	}
}

// The remote having no branch yet is not a pull failure. It happens when the
// first publish ever committed and then failed to push: this clone has a
// commit, the remote is still empty, and treating that as fatal blocks the
// very recovery that would fix it.
func TestIsEmptyRemoteRecognisesAPullWithNoBranchYet(t *testing.T) {
	ctx := context.Background()
	rn := New()
	if err := rn.Available(ctx); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")

	origin := filepath.Join(root, "origin.git")
	if res := rn.run(ctx, "", "init", "--quiet", "--bare", origin); res.Failed() {
		t.Fatalf("init bare: %v\n%s", res.Err, res.Output)
	}
	clone := filepath.Join(root, "clone")
	if res := rn.Clone(ctx, origin, clone); res.Failed() {
		t.Fatalf("clone: %v\n%s", res.Err, res.Output)
	}
	if err := os.WriteFile(filepath.Join(clone, "note.txt"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	rn.Add(ctx, clone, "note.txt")
	rn.Commit(ctx, clone, "local only")

	res := rn.PullFFOnly(ctx, clone)
	if !res.Failed() {
		t.Skip("this git pulls cleanly from an empty remote; nothing to recognise")
	}
	if !IsEmptyRemote(res) {
		t.Errorf("empty remote not recognised, publish would treat it as fatal:\n%s", res.Output)
	}
}

// A credential helper is what hands git ALREADY STORED credentials without
// asking anything. Clearing it — which reads like it belongs among the
// no-prompting settings — leaves git with nothing but a prompt, and every
// HTTPS remote starts failing with "could not read Username". That regression
// shipped once; this is the guard.
//
// Uses whatever remote the developer's own share-repo points at, because the
// bug only shows against a real authenticated one. Skipped when there is none.
func TestCredentialHelperSurvives(t *testing.T) {
	ctx := context.Background()
	rn := New()
	if err := rn.Available(ctx); err != nil {
		t.Skip("git not available")
	}

	for _, f := range nonInteractiveFlags {
		if strings.HasPrefix(f, "credential.helper") {
			t.Fatal("credential.helper is being cleared: stored credentials will stop working for every HTTPS remote")
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	clone := filepath.Join(home, ".engram", "share-repo")
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		t.Skip("no shared-repo clone on this machine")
	}
	if rn.RemoteURL(ctx, clone) == "" {
		t.Skip("clone has no remote")
	}

	// Reaching the remote at all proves credentials still resolve without a
	// prompt. A network outage would fail here too, hence the plain message.
	if res := rn.run(ctx, clone, "ls-remote", "--heads", "origin"); res.Failed() {
		t.Errorf("could not reach the configured remote non-interactively (offline, or credentials are being suppressed):\n%s", res.Output)
	}
}
