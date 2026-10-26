package gitbridge

// Per-repository publishing settings: the name chunks are signed with, and
// what a chunk must not contain before it leaves this machine.
//
//	<data>/engram-ui-repos.json, keyed by the clone's remote URL:
//
//	{"repos": {"git@host:me/memories.git": {
//	    "created_by": "me",
//	    "blocklist": ["work-account", "acme\\.com"]}}}
//
// Edited by hand; nothing in the UI writes it. Blocklist entries are regular
// expressions, matched case-insensitively (see screen.go).
//
// Beside engram.db, like the ledger, because Disconnect deletes the clone:
// settings kept in there would vanish with it, and the next publish would go
// out unscreened under the OS account — silently, and in the unsafe direction.
// Keyed by URL rather than by clone directory because a later repository can
// land in the same directory.
//
// A repository with no entry behaves exactly as before.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const settingsFileName = "engram-ui-repos.json"

// RepoSettings is one repository's entry.
type RepoSettings struct {
	CreatedBy string   `json:"created_by,omitempty"`
	Blocklist []string `json:"blocklist,omitempty"`
}

// settings returns this clone's entry; found is false when it has none. A file
// that exists but cannot be read is an error, never "no settings".
func (b *Bridge) settings(ctx context.Context) (s RepoSettings, found bool, err error) {
	if b.git == nil {
		return RepoSettings{}, false, nil
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(b.dir), settingsFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return RepoSettings{}, false, nil
	}
	if err != nil {
		return RepoSettings{}, false, fmt.Errorf("read %s: %w", settingsFileName, err)
	}
	var file struct {
		Repos map[string]RepoSettings `json:"repos"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return RepoSettings{}, false, fmt.Errorf("parse %s: %w", settingsFileName, err)
	}
	url := b.RemoteURL(ctx)
	if url == "" {
		return RepoSettings{}, false, nil
	}
	s, found = file.Repos[url]
	return s, found, nil
}

// user is the name this clone's chunks are signed with: the repository's
// created_by, else the OS account engram stamps anyway. Unreadable settings fall
// back too: this only answers "which chunks are mine", and publish reads the
// settings itself and refuses on error.
func (b *Bridge) user() string {
	if s, ok, err := b.settings(context.Background()); err == nil && ok {
		if u := strings.TrimSpace(s.CreatedBy); u != "" {
			return u
		}
	}
	return localUser()
}

// patterns compiles the blocklist, case-insensitively.
func (s RepoSettings) patterns() ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(s.Blocklist))
	for _, p := range s.Blocklist {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			return nil, fmt.Errorf("blocklist pattern %q in %s: %w", p, settingsFileName, err)
		}
		out = append(out, re)
	}
	return out, nil
}
