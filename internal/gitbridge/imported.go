package gitbridge

// Which chunks of the shared repo this machine already holds.
//
// "Am I behind?" has no other answer available. engram records the chunks it
// imported in its own sync_chunks table and exposes that nowhere: not over
// HTTP, not through the CLI (an import has no dry-run), and reading its
// database directly is not one of this repo's boundaries. So we keep a ledger
// of the chunks we watched arrive — the ones we imported, and the ones our own
// export wrote — and treat everything else in the manifest as somebody else's
// work we have not applied.
//
// Deliberately biased towards over-reporting. The ledger only knows what
// happened through this panel, so chunks imported another way (the engram CLI
// by hand, or the cloud transport) look pending when they are not. That
// mistake costs one redundant Pull, which imports nothing and corrects the
// ledger on the spot. The opposite mistake — a green "up to date" while a
// teammate's memories are missing — is the exact bug this feature exists to
// kill, so it is the one we refuse to make.
//
// Stored beside the engram data directory, not inside the clone.
//
// It used to live in <clone>/.git/, on the reasoning that it described the
// clone. It does not: it records what this machine's DATABASE has applied and
// what it has published out of that database. Disconnect deletes the clone but
// not the database, so keeping the ledger in there threw away knowledge that
// was still true — and every disconnect/reconnect resurfaced the same
// teammate commits as unapplied and turned the next publish into a full
// re-export, forever.
//
// Living next to engram.db is also the honest scope for the export watermark:
// "what this database has already published" is exactly the question
// stagedView asks. Still never shared — a sibling of the clone, not inside it,
// so git cannot track it and no teammate can receive it.
//
// A ledger from a previous repository is harmless if you connect to a new one:
// chunk ids are content hashes, so nothing matches, and stagedView drops any
// staged id absent from the new manifest — which degrades to a full re-export,
// the safe direction.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const importedFileName = "engram-ui-imported.json"

// importedLedger is the on-disk form. Two buckets per project directory:
//
//	projects  — chunk ids this machine holds (imported here, or written here)
//	published — chunk ids THIS CLONE exported
//
// The second is the narrower one and it exists because "mine" is ambiguous:
// engram stamps chunks with a username, and the same person on a second
// computer produces chunks under the same name. Treating those as ours when
// deciding what to export would hand the other computer's timestamp to our
// export and truncate it — the exact failure this whole staging dance exists to
// prevent. A clone knows what IT published, and that is the only honest answer.
type importedLedger struct {
	Version   int                 `json:"version"`
	Projects  map[string][]string `json:"projects"`
	Published map[string][]string `json:"published,omitempty"`
}

// importedPath puts the ledger next to the clone, not in it: b.dir is
// <data dir>/share-repo*, so this resolves to <data dir>/, alongside engram.db.
func (b *Bridge) importedPath() string {
	return filepath.Join(filepath.Dir(b.dir), importedFileName)
}

// readLedger loads the ledger. Best-effort: a missing, unreadable or corrupt
// file degrades to empty, which over-reports pending work and under-claims what
// we published — both the safe direction.
func (b *Bridge) readLedger() importedLedger {
	l := importedLedger{Version: 1}
	data, err := os.ReadFile(b.importedPath())
	if err != nil {
		return l
	}
	if err := json.Unmarshal(data, &l); err != nil {
		return importedLedger{Version: 1}
	}
	return l
}

// imported returns, per project directory, the set of chunk ids known to be in
// the local database.
func (b *Bridge) imported() map[string]map[string]bool {
	return setsOf(b.readLedger().Projects)
}

// publishedFrom returns the chunk ids this clone exported for one project.
func (b *Bridge) publishedFrom(dirName string) map[string]bool {
	return setsOf(b.readLedger().Published)[dirName]
}

func setsOf(m map[string][]string) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(m))
	for dir, ids := range m {
		set := make(map[string]bool, len(ids))
		for _, id := range ids {
			set[id] = true
		}
		out[dir] = set
	}
	return out
}

// markImported records ids as present locally.
func (b *Bridge) markImported(dirName string, ids []string) {
	b.mark(dirName, ids, false)
}

// markPublished records ids as exported from this clone. Implies present
// locally — a chunk we wrote came out of this database.
func (b *Bridge) markPublished(dirName string, ids []string) {
	b.mark(dirName, ids, true)
}

// ledgerMu serialises mark across every Bridge: all clones share the one
// ledger file (see repos.go), and b.mu only covers its own clone.
var ledgerMu sync.Mutex

// mark is the read-modify-write behind both.
//
// Best-effort on failure: an unwritten ledger costs a redundant pull or a
// redundant full export, never data.
func (b *Bridge) mark(dirName string, ids []string, published bool) {
	if len(ids) == 0 {
		return
	}
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	l := b.readLedger()
	l.Version = 1
	l.Projects = addIDs(l.Projects, dirName, ids)
	if published {
		l.Published = addIDs(l.Published, dirName, ids)
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return
	}
	path := b.importedPath()
	// The data dir normally exists — engram.db is in it — but the ledger no
	// longer rides along with a directory something else creates, so say so.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// addIDs merges ids into one bucket, deduplicated and sorted so the file stays
// stable and diffs readably.
func addIDs(bucket map[string][]string, dirName string, ids []string) map[string][]string {
	if bucket == nil {
		bucket = map[string][]string{}
	}
	set := make(map[string]bool, len(bucket[dirName])+len(ids))
	for _, id := range bucket[dirName] {
		set[id] = true
	}
	for _, id := range ids {
		set[id] = true
	}
	flat := make([]string, 0, len(set))
	for id := range set {
		flat = append(flat, id)
	}
	sort.Strings(flat)
	bucket[dirName] = flat
	return bucket
}

// notMissing returns the chunk ids that need no pull: the ones the ledger
// watched arrive, plus the ones this user exported.
//
// The second half is not a shortcut, it is the definition — a chunk attributed
// to you was written out of this database, so "pull it" is nonsense. It also
// happens to be what makes the column usable from the first render, without
// the ledger having to be seeded: only chunks published by other people can
// possibly be new to you.
//
// Known limit: attribution is a username, not a machine. If you run engram as
// the same user on two computers, this machine treats the other one's chunks
// as already present. Selective pull's "Pull total" is the way in when that is
// your setup, and the panel says so.
//
// Note the deliberate asymmetry with stagedView, which refuses that same
// username shortcut. Being wrong here shows a green tick you did not earn and
// costs one redundant pull; being wrong there drops memories on the floor. So
// the display is allowed to be optimistic and the export is not.
//
// me is the name this clone signs with (Bridge.user), which is not always the
// OS account.
func notMissing(m Manifest, ledger map[string]bool, me string) map[string]bool {
	out := make(map[string]bool, len(m.Chunks))
	for _, c := range m.Chunks {
		if ledger[c.ID] || authorOf(c) == me {
			out[c.ID] = true
		}
	}
	return out
}

// localUser mirrors engram's sync.GetUsername (USER, then USERNAME, then the
// hostname), which is what stamps created_by on every chunk it exports. Copied
// rather than imported — this repo never imports engram packages — so it has to
// track third_party/engram/internal/sync/sync.go if that ever changes. A
// mismatch is not dangerous: it only makes your own chunks look pullable again.
func localUser() string {
	for _, key := range []string{"USER", "USERNAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

// chunkIDs flattens manifest entries to their ids.
func chunkIDs(entries []ChunkEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}
