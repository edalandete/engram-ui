package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/edalandete/engram-ui/internal/client"
	"github.com/edalandete/engram-ui/internal/engramcli"
	"github.com/edalandete/engram-ui/internal/gitbridge"
	"github.com/edalandete/engram-ui/internal/gitcli"
	"github.com/edalandete/engram-ui/internal/launcher"
)

const shareUsage = "usage: engram-ui share pull|push|sync -project NAME"

// Each action gets the budget the panel gives it (handlers.cliCtx).
const shareActionTimeout = 5 * time.Minute

// runShare is `engram-ui share pull|push|sync -project NAME`: the Engram Shared
// panel's pull and publish without the web UI, for scripts and AI assistants.
// Same clones, project→repository assignment and ledger as the panel, so the
// two can be mixed. Connecting and assigning repositories stays in the panel.
//
// shortcut: no lock shared with a running engram-ui; git's index.lock stops
// clashing git work, but two processes writing the ledger at once can drop ids.
// Add a lockfile in the data dir if people start running both together.
func runShare(args []string) error {
	action, project, err := parseShareArgs(args)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// engram serve runs for the length of the command because the conflict gate
	// asks its HTTP API, as the panel does. Its binary (the one next to
	// engram-ui, else the embedded copy) also runs the export and the import.
	token := os.Getenv("ENGRAM_HTTP_TOKEN")
	l := launcher.New()
	url, err := l.Spawn(ctx, token)
	if err != nil {
		return err
	}
	defer func() { _ = l.Stop() }()
	c := client.New(url, token)
	h, err := l.WaitReady(ctx, c, 15*time.Second)
	if err != nil {
		return err
	}
	cli := engramcli.New(l.BinPath(), "")
	engramVersion := cli.Version(ctx)
	if engramVersion == "" {
		engramVersion = h.Version
	}
	if err := checkVersion(engramVersion); err != nil {
		return fmt.Errorf("incompatible engram: %w", err)
	}

	git := gitcli.New()
	if err := git.Available(ctx); err != nil {
		return fmt.Errorf("Engram Shared needs git: %w", err)
	}
	b := gitbridge.NewRepos(cli, git).For(project)
	if b == nil {
		return fmt.Errorf("%s has no shared repository: connect or assign one in engram-ui, Engram Shared", project)
	}
	fmt.Printf("Repository: %s\n", b.RemoteURL(ctx))

	switch action {
	case "pull":
		return sharePull(ctx, b, project)
	case "push":
		return sharePush(ctx, c, b, project)
	}
	// sync. Pull first: publishing never imports teammates' work, and a full
	// re-export is refused while this machine has commits it has not applied.
	if err := sharePull(ctx, b, project); err != nil {
		if !errors.Is(err, gitbridge.ErrEmptyRepo) && !errors.Is(err, gitbridge.ErrNotShared) {
			return err
		}
		fmt.Printf("Nothing to pull: %v\n", errors.Unwrap(err))
	}
	return sharePush(ctx, c, b, project)
}

func parseShareArgs(args []string) (action, project string, err error) {
	if len(args) == 0 {
		return "", "", errors.New(shareUsage)
	}
	action = args[0]
	if action != "pull" && action != "push" && action != "sync" {
		return "", "", fmt.Errorf("unknown share action %q\n%s", action, shareUsage)
	}
	fs := flag.NewFlagSet("share "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&project, "project", "", "engram project")
	if err := fs.Parse(args[1:]); err != nil {
		return "", "", fmt.Errorf("%w\n%s", err, shareUsage)
	}
	project = strings.TrimSpace(project)
	if project == "" || fs.NArg() > 0 {
		return "", "", errors.New(shareUsage)
	}
	return action, project, nil
}

func sharePull(ctx context.Context, b *gitbridge.Bridge, project string) error {
	ctx, cancel := context.WithTimeout(ctx, shareActionTimeout)
	defer cancel()
	res := b.Pull(ctx, project)
	printOutput(res.Output)
	if res.Err != nil {
		return fmt.Errorf("pull failed: %w", res.Err)
	}
	// engram skips the chunks it already has; its output above says what it applied.
	fmt.Printf("Pulled %s (%d commit(s) in the repository)\n", project, res.ChunksTotal)
	return nil
}

func sharePush(ctx context.Context, c *client.Client, b *gitbridge.Bridge, project string) error {
	ctx, cancel := context.WithTimeout(ctx, shareActionTimeout)
	defer cancel()

	// Same gate as the panel: publishing unresolved conflicts replicates
	// contradictory memories to everyone else. Unlike the panel, a failed check
	// blocks too: nobody is looking at a page that would show the conflicts.
	stats, err := c.ConflictStats(ctx, project)
	if err != nil {
		return fmt.Errorf("publish blocked: could not check %s for unresolved conflicts: %w", project, err)
	}
	if n := stats.ByJudgmentStatus["pending"]; n > 0 {
		return fmt.Errorf("publish blocked: %s has %d unresolved conflict(s); resolve them first", project, n)
	}

	if b.FirstPublishHere(project) {
		fmt.Printf("Note: this machine has no record of publishing %s, so this publish re-sends the whole project. "+
			"Expected the first time, and once only; nobody ends up with duplicate observations.\n", project)
	}
	res := b.Push(ctx, project)
	printOutput(res.Output)
	switch {
	case res.Blocked:
		for _, f := range res.Findings {
			fmt.Println(f)
		}
		if len(res.Findings) > 0 {
			fmt.Println("Fix these observations and publish again, or use Publish held-back commit anyway in engram-ui, Engram Shared.")
		}
		return res.Err
	case res.Err != nil:
		stage := res.Stage
		if stage == "" {
			stage = "publish"
		}
		return fmt.Errorf("publish failed during %s: %w", stage, res.Err)
	case res.Pushed:
		fmt.Printf("Published %s\n", project)
	case res.Warn != "":
		fmt.Printf("Nothing was exported for %s\n%s\n", project, res.Warn)
	default:
		fmt.Printf("Nothing new to publish for %s\n", project)
	}
	return nil
}

func printOutput(out string) {
	if strings.TrimSpace(out) != "" {
		fmt.Println(out)
	}
}
