// Package selfupdate replaces engram-ui.exe with the newest release: `engram-ui
// update`. It execs gh, so the user's GitHub login is what grants access to the
// repository. Only engram-ui.exe: the engram.exe beside it is left alone.
package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/semver"
)

const (
	Repo  = "edalandete/engram-ui"
	asset = "engram-ui.exe"
)

// ErrNeedsAdmin means this process cannot write the folder engram-ui.exe is in,
// Program Files above all.
var ErrNeedsAdmin = errors.New("cannot write the folder of engram-ui.exe without administrator rights")

// Latest is the tag of the newest release.
func Latest(ctx context.Context) (string, error) {
	return gh(ctx, "release", "view", "--repo", Repo, "--json", "tagName", "--jq", ".tagName")
}

// Newer reports whether tag is newer than current. A build without a release
// version ("dev") is older than any release.
func Newer(tag, current string) bool {
	return !semver.IsValid(current) || semver.Compare(tag, current) > 0
}

// Install downloads tag's engram-ui.exe and puts it in place of exe.
func Install(ctx context.Context, tag, exe string) error {
	// Probed first so a missing right is ErrNeedsAdmin, not a gh error.
	probe, err := os.CreateTemp(filepath.Dir(exe), ".engram-ui-update-*")
	if errors.Is(err, fs.ErrPermission) {
		return ErrNeedsAdmin
	}
	if err != nil {
		return err
	}
	probe.Close()
	os.Remove(probe.Name())

	tmp := exe + ".new"
	if _, err := gh(ctx, "release", "download", tag, "--repo", Repo, "--pattern", asset, "--output", tmp, "--clobber"); err != nil {
		os.Remove(tmp)
		return err
	}
	return replace(exe, tmp)
}

// replace puts tmp in place of exe. A running exe cannot be overwritten on
// Windows, but it can be renamed: exe goes to exe.old, which the next update
// deletes, unless an engram-ui started from it is still running.
func replace(exe, tmp string) error {
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot move %s aside (is an older engram-ui still running?): %w", exe, err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("cannot install %s (%v) nor put the previous engram-ui back (%v): it is in %s, rename it to %s", tmp, err, rerr, old, exe)
		}
		return err
	}
	return nil
}

func gh(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if errors.Is(err, exec.ErrNotFound) {
		return "", fmt.Errorf("needs the GitHub CLI (gh), signed in with access to %s: %w", Repo, err)
	}
	if err != nil {
		return "", fmt.Errorf("gh %s %s: %v: %s", args[0], args[1], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
