// Package gitcli shells out to the system `git` binary.
//
// Why this exists: engram-ui can share memories through a plain git repo
// instead of the engram cloud server (see internal/gitbridge). engram's local
// sync writes a `.engram/` directory of immutable, content-addressed chunks and
// literally tells you to `git add .engram/` — the format was designed for it.
// Driving that means running git, and git has no library binding we want: the
// system binary already carries the user's credential helper, SSH agent and
// identity, so shelling out is both the smallest and the only sane option.
//
// This is the FOURTH boundary to the outside world, alongside internal/client
// (local engram HTTP), internal/engramcli (engram CLI exec) and
// internal/cloudclient (cloud HTTP read). It is deliberately the only place
// that execs git; handlers must depend on this package, never on os/exec.
//
// Non-interactive by construction: every invocation disables git's credential
// and SSH prompts. A prompt inside an HTTP handler would hang the request until
// the context deadline instead of failing with a readable message.
package gitcli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// Runner execs git in a given working directory.
type Runner struct {
	gitPath string
}

// New returns a Runner using `git` from PATH.
func New() *Runner { return &Runner{gitPath: "git"} }

// Result is the captured outcome of one git invocation. Output is the combined
// stdout+stderr — git writes progress and errors to stderr, and the UI shows
// the whole thing verbatim. Err is non-nil when git exits non-zero.
type Result struct {
	Args   []string
	Output string
	Err    error
}

// Failed reports whether the invocation errored.
func (r Result) Failed() bool { return r.Err != nil }

// run executes git in dir and captures combined output.
func (rn *Runner) run(ctx context.Context, dir string, args ...string) Result {
	cmd := exec.CommandContext(ctx, rn.gitPath, append(append([]string{}, nonInteractiveFlags...), args...)...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = nonInteractiveEnv()

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return Result{Args: args, Output: strings.TrimRight(buf.String(), "\n"), Err: err}
}

// runOut executes git and returns stdout as raw bytes, unmodified. Used for
// plumbing commands whose output is content (`git show`), where trimming or
// re-encoding would corrupt the result.
func (rn *Runner) runOut(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, rn.gitPath, append(append([]string{}, nonInteractiveFlags...), args...)...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = nonInteractiveEnv()

	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			return nil, err
		}
		return nil, errors.New(msg)
	}
	return out.Bytes(), nil
}

// nonInteractiveEnv inherits the process environment and forces git to fail
// rather than prompt. Without this, a repo the user is not authenticated for
// blocks a web request for the whole timeout while something waits for input
// that will never arrive — possibly a dialog on a machine nobody is watching.
//
// The settings, and one that is deliberately NOT here:
//
//   - GIT_TERMINAL_PROMPT=0 — git's own terminal prompt.
//   - SSH BatchMode — passphrase and host-key prompts.
//   - GCM_INTERACTIVE=never — Git Credential Manager is the default on Windows
//     and it is a GUI; this makes it fail instead of opening a dialog nobody
//     is watching.
//   - core.askPass="" — GIT_ASKPASS="" does NOT disable askpass. git tests the
//     variable with `askpass && *askpass`, so an empty string reads as unset
//     and it falls through to core.askPass and then to SSH_ASKPASS. Only the
//     -c config override actually takes, which is why nonInteractiveFlags
//     exists.
//
// NOT credential.helper="". Clearing the helper list looks like it belongs in
// this list and does the opposite: the helper is what supplies ALREADY STORED
// credentials without asking anything. Removing it leaves git with nothing but
// a prompt, so every HTTPS remote failed with "could not read Username for
// 'https://github.com': terminal prompts disabled" — on a repository that had
// been working for weeks. Verified both ways against a real remote.
func nonInteractiveEnv() []string {
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
	)
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return env
}

// nonInteractiveFlags are prepended to every git invocation. See
// nonInteractiveEnv for why the environment alone is not enough.
var nonInteractiveFlags = []string{"-c", "core.askPass="}

// Available reports whether a usable git binary is on PATH.
func (rn *Runner) Available(ctx context.Context) error {
	if res := rn.run(ctx, "", "--version"); res.Failed() {
		return errors.New("git not available on PATH: " + res.Output)
	}
	return nil
}

// Clone clones url into dir. dir must not exist yet.
//
// The `--` is load-bearing, not tidiness. url arrives from a form field, and
// without the separator git parses a leading dash as its own option: passing
// `--upload-pack=<command>` as the URL makes git run that command. Verified
// against git 2.55 — it creates the file and only then reports that it could
// not read from the remote. With `--` the same input is treated as a
// repository name and git says it does not exist, having run nothing.
//
// Rejecting dash-prefixed URLs in the handler would work too, and would be the
// wrong place: this is the function that builds the command line, so this is
// the function that has to be safe no matter who calls it.
func (rn *Runner) Clone(ctx context.Context, url, dir string) Result {
	return rn.run(ctx, "", "clone", "--", url, dir)
}

// RemoteURL returns the origin URL of the clone at dir, or "" if dir is not a
// git repo. This doubles as engram-ui's "is sharing configured?" check: the
// clone IS the configuration, so there is no separate settings file to keep in
// sync with reality.
func (rn *Runner) RemoteURL(ctx context.Context, dir string) string {
	out, err := rn.runOut(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Branch returns the checked-out branch name. On a repo with no commits yet
// git still reports the unborn branch, so this works on a fresh clone of an
// empty repo too. Falls back to "main" when git reports nothing.
func (rn *Runner) Branch(ctx context.Context, dir string) string {
	out, err := rn.runOut(ctx, dir, "branch", "--show-current")
	if err == nil {
		if b := strings.TrimSpace(string(out)); b != "" {
			return b
		}
	}
	return "main"
}

// HasCommits reports whether the repo has any commit. A freshly cloned empty
// repo has none, which makes pull and merge meaningless — callers skip them.
func (rn *Runner) HasCommits(ctx context.Context, dir string) bool {
	_, err := rn.runOut(ctx, dir, "rev-parse", "--verify", "HEAD")
	return err == nil
}

// PullFFOnly fast-forwards the checkout to origin. It deliberately refuses to
// create a merge commit: a non-fast-forward here means local work exists that
// hasn't been reconciled, and the publish flow must stop rather than guess.
func (rn *Runner) PullFFOnly(ctx context.Context, dir string) Result {
	return rn.run(ctx, dir, "pull", "--ff-only", "origin", rn.Branch(ctx, dir))
}

// Fetch updates remote-tracking refs without touching the worktree.
func (rn *Runner) Fetch(ctx context.Context, dir string) Result {
	return rn.run(ctx, dir, "fetch", "origin")
}

// Merge merges ref into the checkout. A non-zero exit usually means conflicts;
// callers inspect Unmerged to decide whether they can resolve them.
func (rn *Runner) Merge(ctx context.Context, dir, ref string) Result {
	return rn.run(ctx, dir, "merge", "--no-edit", ref)
}

// MergeAbort restores the pre-merge state. Used when conflicts land in files
// we don't know how to resolve, so we never leave a half-merged worktree.
func (rn *Runner) MergeAbort(ctx context.Context, dir string) Result {
	return rn.run(ctx, dir, "merge", "--abort")
}

// Unmerged lists conflicted paths (repo-relative, slash-separated).
func (rn *Runner) Unmerged(ctx context.Context, dir string) ([]string, error) {
	out, err := rn.runOut(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	return splitLines(string(out)), nil
}

// ShowStage reads one side of a conflicted file from the index: stage 2 is
// "ours" (the branch being merged into), stage 3 is "theirs".
func (rn *Runner) ShowStage(ctx context.Context, dir string, stage int, path string) ([]byte, error) {
	return rn.runOut(ctx, dir, "show", ":"+itoa(stage)+":"+path)
}

// SetConfig writes one key to the repository's own config (git config --local).
func (rn *Runner) SetConfig(ctx context.Context, dir, key, value string) Result {
	return rn.run(ctx, dir, "config", "--local", key, value)
}

// Add stages the given pathspecs.
func (rn *Runner) Add(ctx context.Context, dir string, pathspecs ...string) Result {
	return rn.run(ctx, dir, append([]string{"add", "--"}, pathspecs...)...)
}

// Commit records the staged changes. Returns Failed() when there is nothing
// staged; callers check IsNothingToCommit to tell that apart from a real error.
func (rn *Runner) Commit(ctx context.Context, dir, message string) Result {
	return rn.run(ctx, dir, "commit", "-m", message)
}

// CommitMerge concludes a merge whose conflicts the caller already resolved and
// staged. Separate from Commit because a merge commit needs no -m (git reuses
// MERGE_MSG) and must not fail on an "empty" diff against the first parent.
func (rn *Runner) CommitMerge(ctx context.Context, dir string) Result {
	return rn.run(ctx, dir, "commit", "--no-edit")
}

// Push publishes the current branch, setting upstream on first push so a
// freshly cloned empty repo works without extra setup.
func (rn *Runner) Push(ctx context.Context, dir string) Result {
	return rn.run(ctx, dir, "push", "--set-upstream", "origin", rn.Branch(ctx, dir))
}

// HasUnpushedCommits reports whether the local branch is ahead of its upstream.
//
// Needed because a publish that commits and then fails to push leaves the work
// on this machine only, and nothing else notices: the next publish exports
// nothing new (it was already committed), sees a clean tree, and reports
// "nothing new to publish" while the observations sit unpushed forever.
//
// No upstream yet means nothing has ever been pushed, so anything committed
// counts as ahead. A repository with no commits at all is not ahead of
// anything.
func (rn *Runner) HasUnpushedCommits(ctx context.Context, dir string) bool {
	if !rn.HasCommits(ctx, dir) {
		return false
	}
	out, err := rn.runOut(ctx, dir, "rev-list", "--count", "@{upstream}..HEAD")
	if err != nil {
		// No upstream configured: never pushed, so a commit is unpushed work.
		return true
	}
	return strings.TrimSpace(string(out)) != "0"
}

// FirstAdders maps every path ever added to the repo to the author of the
// commit that added it first.
//
// %aN, not %an: it honours a .mailmap in the repo, so a team can map the names
// git config produced to whatever they want shown. Merges carry no diff in git
// log by default, so a path a merge brought in stays credited to the commit
// that really added it. --root because log.showRoot=false would otherwise
// silently drop the first commit's files.
func (rn *Runner) FirstAdders(ctx context.Context, dir string) (map[string]string, error) {
	out, err := rn.runOut(ctx, dir, "log", "--reverse", "--root", "--diff-filter=A", "--name-only", "--format=%x00%aN")
	if err != nil {
		return nil, err
	}
	adders := map[string]string{}
	author := ""
	for _, line := range splitLines(string(out)) {
		if strings.HasPrefix(line, "\x00") {
			author = strings.TrimPrefix(line, "\x00")
			continue
		}
		if _, seen := adders[line]; !seen {
			adders[line] = author
		}
	}
	return adders, nil
}

// HasStagedOrUntracked reports whether the given pathspecs have anything to commit.
func (rn *Runner) HasStagedOrUntracked(ctx context.Context, dir string, pathspecs ...string) (bool, error) {
	out, err := rn.runOut(ctx, dir, append([]string{"status", "--porcelain", "--"}, pathspecs...)...)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// IsNothingToCommit recognises git's "nothing to commit" exit, which is a
// normal outcome (someone else already published the same state), not a fault.
func IsNothingToCommit(res Result) bool {
	return strings.Contains(res.Output, "nothing to commit") ||
		strings.Contains(res.Output, "nothing added to commit")
}

// IsRejected recognises a push refused because the remote moved on. This is the
// expected outcome of two people publishing at the same time and triggers the
// manifest merge-and-retry in gitbridge.
func IsRejected(res Result) bool {
	return strings.Contains(res.Output, "[rejected]") ||
		strings.Contains(res.Output, "non-fast-forward") ||
		strings.Contains(res.Output, "fetch first")
}

// IsEmptyRemote recognises a pull against a remote that has no branch yet.
//
// Reachable in one specific way, and it is a trap: the very first publish
// commits locally and then fails to push. The remote stays empty while this
// clone has a commit, so every later publish dies at the pull step with
// "couldn't find remote ref master" — the repair path blocked by the damage it
// exists to repair. Nothing to fast-forward to is not a failure.
func IsEmptyRemote(res Result) bool {
	return strings.Contains(res.Output, "couldn't find remote ref") ||
		strings.Contains(res.Output, "Couldn't find remote ref")
}

func splitLines(s string) []string {
	raw := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
