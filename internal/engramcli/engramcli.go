// Package engramcli shells out to the engram binary for cloud-sync operations
// that the local HTTP API does not expose.
//
// Why this exists: engram's LOCAL server (internal/server) only serves a
// read-only GET /sync/status. The operations that actually move data — push
// (`engram sync --cloud`), pull (`--import`), project enrollment
// (`engram cloud enroll`), and server config (`engram cloud config`) — are CLI
// subcommands with no HTTP trigger. engram-ui already controls the engram
// binary via the launcher, so we drive those subcommands as child processes.
//
// This is the SECOND boundary to engram (the first being internal/client over
// HTTP). It is deliberately the only place that execs the binary; handlers must
// depend on this package, never on os/exec directly.
package engramcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner execs a specific engram binary. binPath is the path the launcher
// extracted (embedded mode); when empty, "engram" on PATH is used (external
// mode). token, when set, is injected as ENGRAM_CLOUD_TOKEN for sync calls so
// a bearer-authenticated cloud works without persisting the token to disk.
type Runner struct {
	binPath string
	token   string
}

func New(binPath, token string) *Runner {
	bin := strings.TrimSpace(binPath)
	if bin == "" {
		bin = "engram"
	}
	return &Runner{binPath: bin, token: strings.TrimSpace(token)}
}

// Result is the captured outcome of one CLI invocation. Output is the combined
// stdout+stderr (engram prints progress to both); it is shown verbatim in the
// UI. Err is non-nil when the process exits non-zero or fails to start.
type Result struct {
	Args   []string
	Output string
	Err    error
}

// Failed reports whether the invocation errored.
func (r Result) Failed() bool { return r.Err != nil }

func (rn *Runner) run(ctx context.Context, args ...string) Result {
	return rn.runIn(ctx, "", args...)
}

// runIn is run() with a controlled working directory. dir="" inherits the
// parent's CWD. A non-empty dir is needed for local-mode import, where engram
// resolves the `.engram/` chunk directory relative to CWD (see LocalImport).
func (rn *Runner) runIn(ctx context.Context, dir string, args ...string) Result {
	return rn.runInEnv(ctx, dir, nil, args...)
}

// runInEnv is runIn with extra environment entries, which win over inherited
// ones of the same name.
func (rn *Runner) runInEnv(ctx context.Context, dir string, extra []string, args ...string) Result {
	cmd := exec.CommandContext(ctx, rn.binPath, args...)
	if dir != "" {
		cmd.Dir = dir
	}

	// Inherit engram-ui's environment so a custom ENGRAM_DATA_DIR (and thus the
	// shared engram.db + cloud.json) resolves identically to the spawned server.
	env := os.Environ()
	if rn.token != "" {
		env = append(env, "ENGRAM_CLOUD_TOKEN="+rn.token)
	}
	cmd.Env = append(env, extra...)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	out := stripUpdateCheckNoise(buf.String())
	out = strings.TrimRight(out, "\n")
	if err != nil && ctx.Err() != nil {
		// Surface cancellation/timeout clearly rather than a bare "signal: killed".
		err = ctx.Err()
	}
	return Result{Args: args, Output: out, Err: err}
}

// Version asks the engram binary what it is, returning "" when it cannot say.
//
// The HTTP /health endpoint cannot answer this: engram's handleHealth returns
// the string literal "0.1.0" regardless of the -X main.version stamped at
// build time, so a version check built on it compares against a constant. The
// CLI does report the real value ("engram v1.16.3-11-ga96efb3"), which is the
// only place the build stamp is visible from outside the process.
//
// Best-effort by design: the caller falls back to /health, and an unknown
// version must not stop the UI from starting.
func (rn *Runner) Version(ctx context.Context) string {
	if rn == nil || strings.TrimSpace(rn.binPath) == "" {
		return ""
	}
	res := rn.run(ctx, "--version")
	if res.Failed() {
		return ""
	}
	for _, line := range strings.Split(strings.ReplaceAll(res.Output, "\r\n", "\n"), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "engram "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// stripUpdateCheckNoise removes engram's update-check preamble from CLI output.
//
// For any subcommand other than `serve`/`mcp`/`cloud serve`, engram runs an
// update check BEFORE the real command and prints the result to stderr (see
// engram cmd/engram/main.go printUpdateCheckResult). Driven as an embedded
// subprocess, that check is pure noise: it never succeeds usefully (a "dev"
// build reports "development builds do not map to a release version"; a tagged
// build hits GitHub's unauthenticated rate limit and reports "403 Forbidden"),
// and the user updates engram by bumping the submodule + rebuilding, not by the
// suggested `brew upgrade`. We capture combined stdout+stderr, so this block
// lands at the very start of the output, ahead of the real command's result.
//
// The preamble is one or more lines starting with a known update-check headline
// ("Could not check for updates:" or "Update available:"), followed by a blank
// separator line that printUpdateCheckResult always emits. We drop everything up
// to and including that blank line.
func stripUpdateCheckNoise(out string) string {
	trimmed := strings.TrimLeft(out, "\n")
	if !strings.HasPrefix(trimmed, "Could not check for updates:") &&
		!strings.HasPrefix(trimmed, "Update available:") {
		return out
	}
	if _, rest, ok := strings.Cut(trimmed, "\n\n"); ok {
		return rest
	}
	// Headline with no trailing blank line (no real command output followed):
	// the whole thing is update-check noise.
	return ""
}

// checkProject rejects a name that engram's argument parser would not read as
// a name.
//
// There is no shell here — exec.Command passes argv directly, so nothing can
// be injected — but a leading dash still turns the value into a flag. The
// sharp edge is `cloud enroll --help`: it prints usage and exits 0, so the
// caller concludes the project was enrolled and moves on to a push that then
// fails somewhere else entirely, for a reason that has nothing to do with the
// real cause.
func checkProject(project string) error {
	p := strings.TrimSpace(project)
	if p == "" {
		return errors.New("project is required")
	}
	if strings.HasPrefix(p, "-") {
		return fmt.Errorf("invalid project name %q: cannot start with a dash", p)
	}
	return nil
}

// SyncPush exports project memories to the configured cloud server. The project
// is auto-enrolled first (enroll is idempotent + local, no network call), so the
// UI never has to make the user reason about enrollment state — a concept engram
// exposes only via CLI, not over HTTP. If enrollment fails the push is aborted
// and that failure is returned.
func (rn *Runner) SyncPush(ctx context.Context, project string) Result {
	if err := checkProject(project); err != nil {
		return Result{Err: err}
	}
	if res := rn.Enroll(ctx, project); res.Failed() {
		return res
	}
	return rn.run(ctx, "sync", "--cloud", "--project", project)
}

// SyncPull imports new remote chunks for the project from the cloud server.
// Auto-enrolls first; see SyncPush for the rationale.
func (rn *Runner) SyncPull(ctx context.Context, project string) Result {
	if err := checkProject(project); err != nil {
		return Result{Err: err}
	}
	if res := rn.Enroll(ctx, project); res.Failed() {
		return res
	}
	return rn.run(ctx, "sync", "--cloud", "--import", "--project", project)
}

// Enroll marks a local project eligible for cloud replication. Local-only:
// flips a SQLite flag, no network call.
func (rn *Runner) Enroll(ctx context.Context, project string) Result {
	if err := checkProject(project); err != nil {
		return Result{Err: err}
	}
	return rn.run(ctx, "cloud", "enroll", project)
}

// Resync forces the next cloud push to re-upload everything local for the
// project. Local-only: clears cloud sync bookkeeping (chunk tracking + un-acks
// the project's mutations), no network call and no data deleted. Use to repair
// divergence after a server reset or interrupted pushes.
func (rn *Runner) Resync(ctx context.Context, project string) Result {
	if err := checkProject(project); err != nil {
		return Result{Err: err}
	}
	return rn.run(ctx, "cloud", "resync", project)
}

// SetServer persists the cloud server URL to ~/.engram/cloud.json.
// Note: engram's `cloud config` rewrites the file with only server_url, so any
// previously persisted token is dropped — the UI passes the token via env
// instead (see Runner.token), so this is not a problem in practice.
func (rn *Runner) SetServer(ctx context.Context, serverURL string) Result {
	if strings.TrimSpace(serverURL) == "" {
		return Result{Err: errors.New("server URL is required")}
	}
	return rn.run(ctx, "cloud", "config", "--server", serverURL)
}

// CloudStatus runs `engram cloud status` and returns its plain-text diagnostic.
func (rn *Runner) CloudStatus(ctx context.Context) Result {
	return rn.run(ctx, "cloud", "status")
}

// LocalImport runs `engram sync --import` in LOCAL mode (no --cloud, no
// --project) with CWD=workDir. engram resolves its chunk source as `./.engram`,
// so workDir must contain a `.engram/` dir with manifest.json + chunks/. It
// applies every chunk listed there to the shared engram.db (resolved via the
// inherited ENGRAM_DATA_DIR env, NOT from CWD). This is how cloudbridge applies
// a hand-picked subset of cloud chunks without engram's all-or-nothing cloud
// import. Local mode has no enrollment/preflight, so it just reads + applies.
func (rn *Runner) LocalImport(ctx context.Context, workDir string) Result {
	if strings.TrimSpace(workDir) == "" {
		return Result{Err: errors.New("workDir is required for local import")}
	}
	return rn.runIn(ctx, workDir, "sync", "--import")
}

// LocalExport runs `engram sync --project X` in LOCAL mode (no --cloud) with
// CWD=workDir. engram writes its chunk output to `./.engram` — manifest.json
// plus one immutable, content-addressed chunk per export — so workDir decides
// which directory receives them. That is what lets a single git clone hold
// several projects side by side: one directory each, one manifest each.
//
// One manifest per project is not a preference, it is required. engram derives
// the incremental export cutoff from the newest chunk in the manifest it finds,
// with no project filter, so two projects sharing a `.engram/` make the second
// one report "all memories already exported" and silently export nothing.
//
// createdBy, when set, is the name stamped on the chunk instead of the OS
// account. engram reads it from USER, then USERNAME (sync.GetUsername), so both
// are overridden for this one process.
func (rn *Runner) LocalExport(ctx context.Context, workDir, project, createdBy string) Result {
	if strings.TrimSpace(workDir) == "" {
		return Result{Err: errors.New("workDir is required for local export")}
	}
	if err := checkProject(project); err != nil {
		return Result{Err: err}
	}
	var env []string
	if u := strings.TrimSpace(createdBy); u != "" {
		env = []string{"USER=" + u, "USERNAME=" + u}
	}
	return rn.runInEnv(ctx, workDir, env, "sync", "--project", project)
}

// CurrentServer reads the persisted cloud server URL from cloud.json so the UI
// can prefill the config form. Returns "" when no server is configured.
// ENGRAM_CLOUD_SERVER env overrides the file, mirroring engram's own
// precedence. Best-effort: any read/parse error yields "".
func CurrentServer() string {
	if v := strings.TrimSpace(os.Getenv("ENGRAM_CLOUD_SERVER")); v != "" {
		return v
	}
	path := cloudConfigPath()
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cc struct {
		ServerURL string `json:"server_url"`
	}
	if err := json.Unmarshal(b, &cc); err != nil {
		return ""
	}
	return strings.TrimSpace(cc.ServerURL)
}

// ClearServer unlinks the cloud server.
//
// The one write this package makes to a file instead of through the CLI, and
// only because the CLI cannot express it: `engram cloud config --server ""`
// exits with "server URL is required" (cloud.go, cmdCloudConfig), so there is
// no subcommand for "no server". Without this the field is a one-way door —
// once set, the UI can only ever point it somewhere else.
//
// Rewrites cloud.json through a generic map so every other key survives, the
// token above all: decoding into a struct and writing it back would silently
// drop whatever engram stores there now or later.
func ClearServer() error {
	if v := strings.TrimSpace(os.Getenv("ENGRAM_CLOUD_SERVER")); v != "" {
		return fmt.Errorf("ENGRAM_CLOUD_SERVER is set to %q in the environment and takes precedence over the config file; unset it and restart engram-ui", v)
	}
	path := cloudConfigPath()
	if path == "" {
		return errors.New("cannot locate cloud.json")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // nothing configured; already unlinked
	}
	if err != nil {
		return err
	}
	cc := map[string]any{}
	if err := json.Unmarshal(raw, &cc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	delete(cc, "server_url")
	out, err := json.MarshalIndent(cc, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the same file holds the cloud token.
	return os.WriteFile(path, out, 0o600)
}

// cloudConfigPath resolves $ENGRAM_DATA_DIR/cloud.json, falling back to
// ~/.engram/cloud.json — the same resolution engram uses.
func cloudConfigPath() string {
	if dir := strings.TrimSpace(os.Getenv("ENGRAM_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "cloud.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".engram", "cloud.json")
}
