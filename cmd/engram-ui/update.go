package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/edalandete/engram-ui/internal/selfupdate"
	"github.com/edalandete/engram-ui/internal/version"
)

const updateUsage = "usage: engram-ui update"

// Room for gh to download ~40 MB over a slow VPN.
const updateTimeout = 5 * time.Minute

// runUpdate is `engram-ui update`: replaces this engram-ui.exe with the newest
// release. When its folder needs administrator rights (Program Files), it runs
// itself again through the UAC prompt and shows what that copy reported.
func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// Set by the administrator relaunch, whose console is hidden: it reports to this file.
	reportPath := fs.String("log", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errors.New(updateUsage)
	}
	if runtime.GOOS != "windows" {
		return errors.New("the releases ship engram-ui.exe for Windows only")
	}
	if *reportPath == "" {
		return update(os.Stdout, false)
	}
	f, err := os.OpenFile(*reportPath, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := update(f, true); err != nil {
		fmt.Fprintln(f, err)
		return err
	}
	return nil
}

func update(out io.Writer, elevated bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, updateTimeout)
	defer cancelTimeout()

	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return err
	}
	tag, err := selfupdate.Latest(ctx)
	if err != nil {
		return err
	}
	if !selfupdate.Newer(tag, version.Version) {
		fmt.Fprintf(out, "Already up to date: %s (latest release %s)\n", version.Version, tag)
		return nil
	}
	err = selfupdate.Install(ctx, tag, exe)
	if errors.Is(err, selfupdate.ErrNeedsAdmin) && !elevated {
		fmt.Fprintf(out, "%s needs administrator rights: asking Windows for them\n", filepath.Dir(exe))
		return updateElevated(out, exe)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s -> %s: %s\nAn engram-ui already running keeps the old version until it is restarted.\n",
		version.Version, tag, exe)
	return nil
}

// updateElevated runs `exe update` as administrator and copies its report to out.
func updateElevated(out io.Writer, exe string) error {
	report, err := os.CreateTemp("", "engram-ui-update-*.log")
	if err != nil {
		return err
	}
	report.Close()
	defer os.Remove(report.Name())
	code, err := selfupdate.RunElevated(exe, []string{"update", "-log", report.Name()})
	if err != nil {
		return err
	}
	if text, err := os.ReadFile(report.Name()); err == nil {
		out.Write(text)
	}
	if code != 0 {
		return fmt.Errorf("the administrator update failed (exit code %d)", code)
	}
	return nil
}
