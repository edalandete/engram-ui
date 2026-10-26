package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/semver"

	"github.com/edalandete/engram-ui/internal/client"
	"github.com/edalandete/engram-ui/internal/cloudbridge"
	"github.com/edalandete/engram-ui/internal/config"
	"github.com/edalandete/engram-ui/internal/engramcli"
	"github.com/edalandete/engram-ui/internal/gitbridge"
	"github.com/edalandete/engram-ui/internal/gitcli"
	"github.com/edalandete/engram-ui/internal/launcher"
	"github.com/edalandete/engram-ui/internal/version"
	"github.com/edalandete/engram-ui/internal/web/handlers"
)

// main exists only to turn an error into an exit code. Everything real is in
// run, because log.Fatalf calls os.Exit and os.Exit does not run deferred
// functions — and one of those defers is what kills the engram subprocess.
//
// The failure that made this necessary is not exotic: start engram-ui twice.
// The second one spawns its own engram, then cannot bind 127.0.0.1:7438
// because the first one holds it, and exits — leaving a live engram serve
// behind, on a port nobody knows, holding the same SQLite file. Repeat and
// they accumulate.
func main() {
	logger := log.New(os.Stderr, "engram-ui ", log.LstdFlags|log.Lmsgprefix)
	if err := run(logger); err != nil {
		logger.Print(err)
		os.Exit(1)
	}
}

func run(logger *log.Logger) error {
	cfg := config.FromFlags()
	flag.Parse()

	logger.Printf("version=%s commit=%s listen=%s", version.Version, version.Commit, cfg.ListenAddr)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	l := launcher.New()
	if cfg.SpawnEngram {
		url, err := l.Spawn(ctx, cfg.EngramToken)
		if err != nil {
			logger.Printf("spawn-engram failed (%v); falling back to external engram at %s", err, cfg.EngramURL)
		} else {
			cfg.EngramURL = url
			logger.Printf("spawned embedded engram at %s", url)
			defer func() { _ = l.Stop() }()
		}
	}

	c := client.New(cfg.EngramURL, cfg.EngramToken)

	h, err := l.WaitReady(ctx, c, 15*time.Second)
	if err != nil {
		return fmt.Errorf("engram not reachable at %s: %w\nstart it with: engram serve", cfg.EngramURL, err)
	}

	// CLI runner drives engram cloud subcommands (push/pull/enroll/config) that
	// have no HTTP trigger. Uses the launcher's binary in embedded mode, else
	// falls back to "engram" on PATH.
	cli := engramcli.New(l.BinPath(), cfg.CloudToken)
	bridge := cloudbridge.New(cli)

	// Ask the binary, not the server. engram's /health hands back the literal
	// "0.1.0" whatever it was built from, so a check against it compares
	// MinEngram to a constant and would fail for everyone the moment MinEngram
	// moved past 0.1.0 — including on the newest engram there is.
	engramVersion := cli.Version(ctx)
	if engramVersion == "" {
		engramVersion = h.Version
	}
	if err := checkVersion(engramVersion); err != nil {
		return fmt.Errorf("incompatible engram: %w", err)
	}
	logger.Printf("engram=%s status=%s url=%s", engramVersion, h.Status, cfg.EngramURL)

	// Sharing through a git repo is an alternative to the cloud server: engram's
	// local sync already writes an immutable, content-addressed chunk directory
	// that git stores well. Needs the system git binary; without it the /share
	// screen explains itself and the rest of the UI is unaffected.
	var share *gitbridge.Repos
	git := gitcli.New()
	if err := git.Available(ctx); err != nil {
		logger.Printf("shared-repo sync disabled: %v", err)
	} else {
		share = gitbridge.NewRepos(cli, git)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handlers.New(c, cli, bridge, share, cfg.CloudToken).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		// WriteTimeout is deliberately unset. /backup, /export.json,
		// /export.csv and /export-vault stream a whole database to the
		// browser and legitimately take as long as they take; a write
		// deadline here would cut the big exports, which is precisely the
		// silent truncation this audit already fixed once in client.Export.
	}

	// Shutdown closes the listeners immediately, so ListenAndServe returns
	// while handlers are still running. Publishing runs git and the engram CLI
	// for seconds at a time; returning from run() here would fire the deferred
	// Stop and kill engram mid-write. So the grace period is actually waited
	// for, which is what makes it a grace period rather than decoration.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutCtx, cancelShut := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShut()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Printf("shutdown: %v (in-flight work was cut short)", err)
		}
	}()

	// Bind explicitly instead of letting ListenAndServe do it, so the browser
	// only opens once there is something to open. It used to launch from a
	// goroutine started before the bind, which meant a port already in use
	// still popped a tab at a dead address — and the tab was timed against the
	// bind with a 300 ms Sleep, a guess rather than a signal.
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	uiURL := "http://" + cfg.ListenAddr
	logger.Printf("serving %s", uiURL)
	if cfg.OpenBrowser {
		go openBrowser(logger, uiURL)
	}

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve: %w", err)
	}
	<-shutdownDone
	return nil
}

func checkVersion(got string) error {
	if version.MinEngram == "v0.0.0" {
		return nil // no requirement declared; see internal/version
	}
	if got == "" {
		return fmt.Errorf("engram returned empty version; expected >= %s", version.MinEngram)
	}
	if !semver.IsValid(got) {
		got = "v" + got
	}
	if !semver.IsValid(got) {
		return fmt.Errorf("engram version %q is not semver", got)
	}
	// Compare release numbers only. build-engram.ps1 stamps the version from
	// `git describe`, which produces things like v1.16.3-11-ga96efb3, and
	// semver reads everything after the dash as a PRE-RELEASE — ordering it
	// BELOW v1.16.3. Without this, a build eleven commits newer than the tag
	// reports as older than the tag.
	if semver.Compare(releaseOf(got), releaseOf(version.MinEngram)) < 0 {
		return fmt.Errorf("engram %s is older than required %s", got, version.MinEngram)
	}
	return nil
}

// releaseOf drops any pre-release suffix, so a build stamped by `git describe`
// compares as the tag it was cut from.
func releaseOf(v string) string {
	if pre := semver.Prerelease(v); pre != "" {
		v = strings.TrimSuffix(semver.Canonical(v), pre)
	}
	return semver.Canonical(v)
}

// openBrowser opens the UI. Called only after the listener is bound, so there
// is no sleep here guessing when the server might be up.
func openBrowser(logger *log.Logger, url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// Reported, not swallowed: a missing xdg-open is the whole reason nothing
	// happened, and silence makes it look like the server never started.
	if err := cmd.Start(); err != nil {
		logger.Printf("could not open a browser (%v) — open %s yourself", err, url)
	}
}
