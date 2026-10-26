// Package gitbridge shares engram memories through a plain git repository
// instead of the engram cloud server.
//
// # Why this works at all
//
// engram's local sync already emits a git-shaped artifact. `engram sync
// --project X` writes `.engram/manifest.json` plus `chunks/<sha256[:8]>.jsonl.gz`,
// where every chunk is immutable and named after its own content hash, and it
// prints "Add to git: git add .engram/" when it finishes. Chunks are therefore
// pure additions — git never has to merge one — and import is idempotent twice
// over, deduplicating by chunk id and again by observation sync_id. A clone is
// a complete backup, and `git log` is an audit trail the cloud server does not
// keep.
//
// # Layout
//
// One directory per project inside a shared repo (several repos can be
// connected at once, each project publishing to one of them — see Repos):
//
//	<clone>/
//	├── .gitattributes
//	├── project-a/.engram/{manifest.json,chunks/*.jsonl.gz}
//	└── project-b/.engram/{manifest.json,chunks/*.jsonl.gz}
//
// The per-project split is mandatory, not cosmetic — see engramcli.LocalExport
// for the cutoff bug that makes a shared `.engram/` drop data. It also shrinks
// the conflict surface to nothing for the common case: two people publishing
// different projects touch different files.
//
// # Nobody exports into the shared directory
//
// engram's file export decides what is new by timestamp: it reads the manifest
// of the directory it exports into, takes the newest created_at in it, and
// keeps only rows written after that (sync.go, lastChunkTime + filterNewData).
// The watermark therefore belongs to the DIRECTORY, and engram assumes exactly
// one writer per directory — an assumption a shared repo breaks by definition.
// Exporting straight into the project folder means a teammate's publish moves
// your watermark forward, so your next publish carries only what you wrote
// since THEIR publish, with no error and no warning: the first contributor
// exports everything (empty manifest) and the second one commits a chunk of
// zero memories.
//
// So publishing never exports into the repo. It builds a private `.engram/` in
// a temp directory containing only this contributor's own chunks, exports
// there, and copies the resulting chunk back, unioning its manifest entry in.
// Each contributor gets the single-writer directory engram expects, and the
// repo's manifest is only ever grown, never handed to engram.
//
// # The publish order is load-bearing
//
// Pull, then export, then commit, then push — always, with no way to skip the
// pull. Exporting before pulling produces a manifest conflict, and the natural
// way to "fix" that (reset the manifest to the remote's) permanently breaks
// sharing: engram records exported chunks in its sync_chunks table keyed by
// target "local", so once an entry is dropped from the manifest the recomputed
// chunk hashes as a duplicate, `engram sync` answers "all memories already
// exported", and those memories never reach the repo again. Nothing in the
// engram CLI repairs that state (`cloud resync` only clears cloud bookkeeping).
// So this package never resets or checks out a manifest: the only reconciliation
// it performs is a union.
//
// # What this transport does not carry
//
// Hard deletes, session deletes and prompt deletes do not propagate. The file
// path derives changes from exported rows, and a hard-deleted row is simply
// gone, so other machines keep their copy forever. Soft-deleted observations do
// propagate. Chunks are gzip, not encryption — anyone with repo access reads
// everything, and observations with scope "personal" are exported alongside the
// rest. Use a private repo.
package gitbridge

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edalandete/engram-ui/internal/engramcli"
	"github.com/edalandete/engram-ui/internal/gitcli"
)

const (
	syncDirName   = ".engram"
	manifestName  = "manifest.json"
	chunksDirName = "chunks"

	// unknownUser labels chunks whose created_by is empty, so they remain
	// selectable in the contributor filter instead of silently matching all.
	unknownUser = "(unknown)"

	// maxPushAttempts bounds the reconcile-and-retry loop when other people are
	// publishing concurrently. Each retry costs one fetch+merge.
	maxPushAttempts = 3
)

// gitAttributes keeps git's platform-specific line-ending rewriting away from
// the payload. Chunks are detected as binary in practice, but saying so is free
// insurance; the manifest is real text and is pinned to LF so Windows and Unix
// clients don't fight over it.
const (
	gitAttributesName = ".gitattributes"
	gitAttributes     = "*.jsonl.gz binary\n.engram/manifest.json text eol=lf\n"
)

// ChunkEntry is one row of engram's sync manifest. Mirrored here rather than
// imported: this repo talks to engram over HTTP and the CLI, never by importing
// its packages. Field names match engram's JSON exactly.
type ChunkEntry struct {
	ID        string `json:"id"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	Sessions  int    `json:"sessions"`
	Memories  int    `json:"memories"`
	Prompts   int    `json:"prompts"`
}

// Manifest is engram's chunk index.
type Manifest struct {
	Version int          `json:"version"`
	Chunks  []ChunkEntry `json:"chunks"`
}

// UserCount reports one contributor's share of a project.
type UserCount struct {
	User   string
	Chunks int
	// Memories counts the distinct memories this person was the first to
	// publish, so the contributors of a project sum to exactly its total.
	Memories int
	// New counts those of them this machine has not imported yet.
	New int
}

// ProjectInfo describes one project directory found in the shared repo.
type ProjectInfo struct {
	Name         string
	Chunks       int
	Contributors []string
	LastUpdate   string
	// Memories is the number of DISTINCT memories in the project, counted by
	// opening the chunks — not by adding up the manifest's per-chunk counters.
	// Those counters describe chunks, and chunks overlap heavily: a re-export
	// republishes memories that other chunks already carry, so the sum can be
	// three times the truth. A number nobody can reconcile with their dashboard
	// is worse than no number, so this one is the real thing.
	Memories int
	// NewChunks/NewMemories are what this machine is missing: chunks the ledger
	// has not seen, and the memories in them that no imported chunk already
	// carries. The second is the honest one — a chunk of 284 memories where you
	// already have 235 of them is 49 memories of work, not 284.
	NewChunks   int
	NewMemories int
	// Silent lists contributors who have published to this project without a
	// single memory ever landing. See silentContributors.
	Silent []string
}

// Bridge orchestrates git and the engram CLI. It owns exactly one clone,
// located under the engram data directory.
type Bridge struct {
	cli *engramcli.Runner
	git *gitcli.Runner
	dir string

	// mu serialises the operations that write to the clone. There is one
	// working directory shared by every request, so two publishes at once
	// would collide on git's index.lock, or worse, interleave an export with
	// a commit and publish a manifest that doesn't match its chunks. Two
	// browser tabs are enough to cause it, so the guard belongs here rather
	// than in the handler. Reads (Projects, Contributors) deliberately do not
	// take the lock: they only parse a manifest, and blocking a page render
	// behind a slow publish would be worse than briefly skipping a directory
	// whose manifest is mid-write.
	mu sync.Mutex

	// refreshedAt is when the clone was last fast-forwarded, guarded by mu.
	// Only read to skip a redundant network round trip; see RefreshIfStale.
	refreshedAt time.Time

	// syncIDs caches the memories each chunk carries, keyed by chunk id.
	// Counting real memories means opening every chunk, which a page render
	// cannot afford to repeat. It needs no invalidation and no TTL: a chunk id
	// is the hash of its contents, so an entry can never go stale — a changed
	// chunk is a different chunk.
	syncIDsMu sync.Mutex
	syncIDs   map[string][]chunkMemory
}

// chunkMemory is one observation as seen inside a chunk: enough to count it and
// to know whether it still exists.
type chunkMemory struct {
	ID      string
	Deleted bool
}

// New returns a Bridge for the clone at RepoDir().
func New(cli *engramcli.Runner, git *gitcli.Runner) *Bridge {
	return &Bridge{cli: cli, git: git, dir: RepoDir(), syncIDs: map[string][]chunkMemory{}}
}

// RepoDir resolves $ENGRAM_DATA_DIR/share-repo, falling back to
// ~/.engram/share-repo — the same base directory engram itself uses, so the
// clone travels with the database it mirrors.
func RepoDir() string {
	if dir := strings.TrimSpace(os.Getenv("ENGRAM_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "share-repo")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "share-repo"
	}
	return filepath.Join(home, ".engram", "share-repo")
}

// Dir returns the local clone path.
func (b *Bridge) Dir() string { return b.dir }

// RemoteURL returns the configured repo, or "" when sharing isn't set up. The
// clone is the configuration — there is no settings file that could disagree
// with what is actually on disk.
func (b *Bridge) RemoteURL(ctx context.Context) string {
	return b.git.RemoteURL(ctx, b.dir)
}

// Setup clones url into the managed directory. Authentication is whatever the
// user's git already uses (credential helper, SSH agent); engram-ui never sees
// or stores a token. Prompts are disabled, so an unauthenticated repo fails
// fast with git's own message instead of hanging.
func (b *Bridge) Setup(ctx context.Context, url string) gitcli.Result {
	b.mu.Lock()
	defer b.mu.Unlock()

	url = strings.TrimSpace(url)
	if url == "" {
		return gitcli.Result{Err: errors.New("repository URL is required")}
	}
	if err := b.git.Available(ctx); err != nil {
		return gitcli.Result{Err: err}
	}
	if existing := b.RemoteURL(ctx); existing != "" {
		return gitcli.Result{Err: fmt.Errorf("already connected to %s — disconnect first", existing)}
	}
	// A leftover directory from a failed clone would make git refuse.
	if entries, err := os.ReadDir(b.dir); err == nil && len(entries) == 0 {
		_ = os.Remove(b.dir)
	}
	return b.git.Clone(ctx, url, b.dir)
}

// Disconnect deletes the local clone. Published memories stay in the remote and
// every imported memory stays in the local engram database; only the working
// copy goes away.
func (b *Bridge) Disconnect() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.dir == "" {
		return errors.New("no repository directory")
	}
	return os.RemoveAll(b.dir)
}

// scan is what the panel actually shows: the real contents of a project,
// derived from the chunks rather than from the manifest's bookkeeping.
//
// One pass in manifest order (chronological) answers everything at once —
// distinct memories, how many of them are missing here, who published what, and
// the sync_id → contributor index — because they are all the same question
// asked of the same set. Memories are credited to the FIRST chunk that carried
// them, which is what makes the per-contributor counts disjoint and their sum
// equal to the project total.
type scan struct {
	Owner    map[string]string // sync_id → the contributor who first published it
	Memories int               // distinct memories in the repo
	New      int               // of those, the ones no imported chunk carries
	Users    []UserCount
}

func (b *Bridge) scanProject(dirName string, m Manifest, known map[string]bool) scan {
	owner := make(map[string]string) // sync_id → first publisher
	gone := make(map[string]bool)    // sync_id → deleted, as of the newest chunk that mentions it
	have := make(map[string]bool)    // sync_id → carried by a chunk this machine imported
	byUser := map[string]*UserCount{}
	order := []string{}

	for _, c := range m.Chunks {
		user := authorOf(c)
		u := byUser[user]
		if u == nil {
			u = &UserCount{User: user}
			byUser[user] = u
			order = append(order, user)
		}
		u.Chunks++

		for _, mem := range b.chunkMemories(dirName, c.ID) {
			if _, seen := owner[mem.ID]; !seen {
				owner[mem.ID] = user
			}
			// Deletion is last-writer-wins, matching what an import would do:
			// the newest chunk mentioning a memory states its current state, and
			// a soft delete published later removes it for everyone.
			gone[mem.ID] = mem.Deleted
			if known[c.ID] {
				have[mem.ID] = true
			}
		}
	}

	// Second pass, because "is it still there" and "does anyone here have it"
	// are only settled once every chunk has spoken.
	out := scan{Owner: owner}
	for id, user := range owner {
		if gone[id] {
			continue // deleted memories are not what the dashboard counts
		}
		out.Memories++
		byUser[user].Memories++
		if !have[id] {
			out.New++
			byUser[user].New++
		}
	}

	sort.Strings(order)
	out.Users = make([]UserCount, 0, len(order))
	for _, user := range order {
		out.Users = append(out.Users, *byUser[user])
	}
	return out
}

// chunkMemories returns the memories one chunk carries, reading it at most once
// per process. A missing or corrupt chunk contributes nothing rather than
// failing the render — and caching that nothing is safe, because a chunk id is
// the hash of its contents, so a chunk can never change under us.
func (b *Bridge) chunkMemories(dirName, chunkID string) []chunkMemory {
	b.syncIDsMu.Lock()
	defer b.syncIDsMu.Unlock()
	if b.syncIDs == nil {
		b.syncIDs = map[string][]chunkMemory{}
	}
	// Keyed by project AND id. A chunk id is sha256 truncated to eight hex
	// digits — 32 bits — so two projects in the same repo colliding is not a
	// theoretical concern at the scale of a long-lived team repo, and the
	// consequence would be one project's memories counted as another's.
	key := dirName + "/" + chunkID
	if mems, ok := b.syncIDs[key]; ok {
		return mems
	}
	path := filepath.Join(b.dir, dirName, syncDirName, chunksDirName, chunkID+".jsonl.gz")
	mems := readChunkMemories(path)
	// Only cache a real read. readChunkMemories returns nil both for "this
	// chunk genuinely carries nothing" and for "the file is missing or
	// truncated", and caching the second is permanent: Projects() runs while
	// PullFFOnly is rewriting the checkout, so it can see a manifest entry
	// before the .jsonl.gz lands. Caching that nil under-reports what is
	// missing until the process restarts — the exact failure the ledger's
	// design note says it refuses to make.
	if mems == nil {
		if _, err := os.Stat(path); err != nil {
			return nil
		}
	}
	b.syncIDs[key] = mems
	return mems
}

// Refresh fast-forwards the clone so a following read reflects what other
// people have published.
//
// Discovery is a directory scan and nothing else in a page render talks to the
// remote, so without this the panel shows the repo as it was the last time THIS
// machine published or pulled. Somebody else's commit then stays invisible, and
// the only thing that would reveal it — pulling that project — is exactly what
// you have no reason to do while the table claims nothing changed.
//
// Deliberately forgiving: a repo nobody has published to yet is not an error,
// so a caller rendering a page can always fall back to the scan and warn only
// when git really failed. Takes the publish lock, because the clone is a single
// working directory and a fetch racing an export is the interleaving that lock
// exists to prevent.
func (b *Bridge) Refresh(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.git.HasCommits(ctx, b.dir) {
		return nil
	}
	err := b.refresh(ctx)
	if err == nil {
		b.refreshedAt = time.Now()
	}
	return err
}

// refreshWindow is how long a fast-forward is considered current enough to
// skip. Measured at ~2.7 s against GitHub, paid on every single render of the
// panel — so navigating away and back, or landing here twice in a minute, used
// to mean waiting for the network twice for the same answer.
//
// Short on purpose. The whole point of refreshing is noticing that a teammate
// published, and half a minute of staleness costs nothing: every action on
// this page (publish, pull) fast-forwards as part of its own work anyway.
const refreshWindow = 30 * time.Second

// RefreshIfStale fast-forwards unless it was done recently. Same contract as
// Refresh otherwise.
func (b *Bridge) RefreshIfStale(ctx context.Context) error {
	b.mu.Lock()
	fresh := !b.refreshedAt.IsZero() && time.Since(b.refreshedAt) < refreshWindow
	b.mu.Unlock()
	if fresh {
		return nil
	}
	return b.Refresh(ctx)
}

// Projects lists the project directories present in the clone. Discovery is
// just "directories containing .engram/manifest.json" — no index file to write,
// and nothing that can drift from the actual contents. It reads the clone as it
// is on disk: call Refresh first if the caller needs the remote's current state.
func (b *Bridge) Projects(ctx context.Context) ([]ProjectInfo, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil, err
	}
	ledger := b.imported()
	me := b.user()
	by := b.committers(ctx)
	out := make([]ProjectInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		m, err := readManifest(b.manifestPath(e.Name()))
		if err != nil {
			continue // not a project directory
		}
		info := ProjectInfo{Name: e.Name(), Chunks: len(m.Chunks)}
		known := notMissing(m, ledger[e.Name()], me)
		for _, c := range m.Chunks {
			if c.CreatedAt > info.LastUpdate {
				info.LastUpdate = c.CreatedAt
			}
			if !known[c.ID] {
				info.NewChunks++
			}
		}
		shown := attributed(e.Name(), m, by)
		s := b.scanProject(e.Name(), shown, known)
		info.Memories, info.NewMemories = s.Memories, s.New
		for _, u := range s.Users {
			info.Contributors = append(info.Contributors, u.User)
		}
		info.Silent = silentContributors(shown)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FirstPublishHere reports whether the next publish of this project will
// re-export it whole rather than only what changed.
//
// stagedView derives the export watermark from the chunks THIS machine
// published, so an empty `published` bucket stages nothing, which sets the
// watermark to zero. That is correct and invisible when the project is new to
// the repository, and misleading when it is not: the button says "sends what
// you have written since your last publish" and instead lands the entire
// project again. The bucket is empty on a fresh clone, on a machine that only
// ever pulled, and on any that published before the ledger existed — so this
// is not an edge case, it is everyone's next publish once.
//
// Only the misleading case is worth a warning, hence the manifest check. Asks
// stagedView rather than the bucket: the ledger is shared by every repository,
// so chunks published to another one fill the bucket and still stage nothing.
func (b *Bridge) FirstPublishHere(project string) bool {
	dirName := ProjectDirName(project)
	m, err := readManifest(b.manifestPath(dirName))
	if err != nil || len(m.Chunks) == 0 {
		return false
	}
	return len(b.stagedView(dirName)) == 0
}

// UnappliedCommits counts commits in the repo this clone has not applied.
//
// It is the precondition for a full re-export doing damage. Republishing sends
// your whole copy of the project, and engram's import is last-writer-wins with
// no timestamp comparison (applyObservationUpsertTx), so any observation
// someone corrected while you were behind gets replaced by your stale version
// for everyone who pulls afterwards. You can only hold a stale version if
// there is a commit you have not applied — hence this count.
//
// Cheap on purpose: manifest plus ledger, no chunk is opened. That is what
// makes it affordable on the publish path as well as on render.
func (b *Bridge) UnappliedCommits(project string) int {
	return b.unappliedCommits(ProjectDirName(project))
}

// unappliedCommits is the same by directory name, for callers inside publish
// that already resolved it — and that hold b.mu, which this must not take.
func (b *Bridge) unappliedCommits(dirName string) int {
	m, err := readManifest(b.manifestPath(dirName))
	if err != nil {
		return 0
	}
	known := notMissing(m, b.imported()[dirName], b.user())
	n := 0
	for _, c := range m.Chunks {
		if !known[c.ID] {
			n++
		}
	}
	return n
}

// Size reports the clone's footprint on disk in bytes, .git included. Worth
// showing because this repo only ever grows: chunks are immutable and nothing
// prunes them, so every publish adds and none removes. Best-effort — anything
// unreadable counts as zero rather than failing the page.
func (b *Bridge) Size() int64 {
	var total int64
	_ = filepath.WalkDir(b.dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Contributors returns the per-user chunk counts for a project in the clone.
// Unlike the cloud equivalent this needs no network and no chunk downloads —
// the manifest is already on disk.
func (b *Bridge) Contributors(ctx context.Context, project string) ([]UserCount, error) {
	dirName := ProjectDirName(project)
	m, err := readManifest(b.manifestPath(dirName))
	if err != nil {
		return nil, err
	}
	shown := attributed(dirName, m, b.committers(ctx))
	return b.scanProject(dirName, shown, notMissing(m, b.imported()[dirName], b.user())).Users, nil
}

// AuthorIndex maps sync_id → the contributor who published it, for every
// observation this project has in the shared repo.
//
// engram stores an author nowhere near an observation: the database column
// closest to provenance is a boolean (synced_from_remote), and identity exists
// only on the sync chunk, as whoever ran the export. So attribution has to be
// reconstructed by opening the chunks — the same thing cloudbridge does, minus
// the network, because here the chunks are already files in the clone.
//
// Attribution is chunk-level, not memory-level, and the FIRST chunk to carry a
// memory wins. The manifest is ordered by created_at, so that means the person
// who first published it.
//
// First rather than last, because a chunk contains everything its publisher's
// database held, not just what they wrote — pull a project and republish it and
// your chunk carries the other person's memories too. Under last-wins a single
// re-export would reassign a whole project's authorship to whoever ran it,
// which is precisely the question this index exists to answer. The cost is that
// a later correction by someone else keeps the original name; the corrected
// text still propagates, only the credit stays put.
//
// Errors are swallowed per chunk. A partial index labels fewer observations,
// which is a lesser evil than a screen that refuses to render because one file
// in a shared repo is truncated.
func (b *Bridge) AuthorIndex(ctx context.Context, project string) (map[string]string, error) {
	dirName := ProjectDirName(project)
	m, err := readManifest(b.manifestPath(dirName))
	if err != nil {
		return nil, err
	}
	return b.scanProject(dirName, attributed(dirName, m, b.committers(ctx)), nil).Owner, nil
}

// committers lazily maps a chunk's repo path to the author of the commit that
// added it. Lazy because it is only a fallback (see attributed): the normal
// repo never needs it, so the normal render never pays for the `git log`.
//
// nil map when git is unavailable: everything falls back to created_by.
func (b *Bridge) committers(ctx context.Context) func() map[string]string {
	return sync.OnceValue(func() map[string]string {
		if b.git == nil {
			return nil
		}
		by, err := b.git.FirstAdders(ctx, b.dir)
		if err != nil {
			return nil
		}
		return by
	})
}

// attributed returns a copy of m in which every chunk whose created_by names
// no one carries its committer instead.
//
// created_by is the OS username of the process that ran the export, which is
// the right label — the team knows each other by username — except when that
// process was not a person: engram running as LocalSystem on Windows stamps
// the machine account ("4A5B6A7CFB1B$"), and engram's last resort is the
// literal "unknown". Only those fall back to the commit author.
//
// For display and filtering only. created_by stays the identity for "is this
// chunk mine?" (notMissing, ownChunkIDs): it is what this machine's engram
// stamps, and the only thing it can be compared with.
func attributed(dirName string, m Manifest, by func() map[string]string) Manifest {
	chunks := append([]ChunkEntry(nil), m.Chunks...)
	for i, c := range chunks {
		if !namesNoOne(c.CreatedBy) {
			continue
		}
		if who := by()[dirName+"/"+syncDirName+"/"+chunksDirName+"/"+c.ID+".jsonl.gz"]; who != "" {
			chunks[i].CreatedBy = who
		}
	}
	return Manifest{Version: m.Version, Chunks: chunks}
}

// namesNoOne recognises a created_by that is not a username: empty, engram's
// "unknown", or a Windows machine account (trailing $). engram's hostname
// fallback is indistinguishable from a username and stays as is.
func namesNoOne(createdBy string) bool {
	u := strings.TrimSpace(createdBy)
	return u == "" || u == "unknown" || strings.HasSuffix(u, "$")
}

// readChunkMemories reads one gzipped chunk and returns every observation in
// it. A chunk is a single gzipped JSON document, despite the .jsonl.gz name
// engram gives it.
//
// deleted_at is carried along because chunks keep soft-deleted memories — that
// is how a deletion reaches other machines — and counting them would put the
// panel permanently above the dashboard, which filters them out.
func readChunkMemories(path string) []chunkMemory {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil
	}
	defer func() { _ = gz.Close() }()

	var chunk struct {
		Observations []struct {
			SyncID    string  `json:"sync_id"`
			DeletedAt *string `json:"deleted_at"`
		} `json:"observations"`
	}
	if err := json.NewDecoder(gz).Decode(&chunk); err != nil {
		return nil
	}
	out := make([]chunkMemory, 0, len(chunk.Observations))
	for _, o := range chunk.Observations {
		id := strings.TrimSpace(o.SyncID)
		if id == "" {
			continue
		}
		out = append(out, chunkMemory{ID: id, Deleted: o.DeletedAt != nil && strings.TrimSpace(*o.DeletedAt) != ""})
	}
	return out
}

// ── publish ───────────────────────────────────────────────────────────────

// PushResult is the outcome of a publish, shaped for the UI banner.
type PushResult struct {
	Project string
	// Stage names the step that failed, so the banner can say something more
	// useful than "git error": "pull" | "export" | "commit" | "push".
	Stage    string
	Output   string
	Exported bool
	Pushed   bool
	// Warn carries a non-fatal diagnosis, currently the adoption trap where a
	// project was already exported to some other .engram/ directory.
	Warn string
	// Blocked marks a refusal rather than a failure: nothing was published and
	// nothing is broken, the request was unsafe. Err carries the reason.
	Blocked bool
	// Findings is what the screening refused (screen.go), when that was the
	// reason for Blocked.
	Findings []Finding
	Err      error
}

// Push publishes what this machine has written since its own last publish,
// escalating to a full re-export by itself when that is the only thing that
// would work. There is no second button: see publish for when it escalates and
// why the user was never in a position to make that call.
func (b *Bridge) Push(ctx context.Context, project string) PushResult {
	return b.publish(ctx, project)
}

// publish runs the four steps — pull, export, commit, push. The pull is first
// and unconditional; see the package comment for why skipping it corrupts
// sharing.
func (b *Bridge) publish(ctx context.Context, project string) PushResult {
	b.mu.Lock()
	defer b.mu.Unlock()

	res := PushResult{Project: project}
	if strings.TrimSpace(project) == "" {
		res.Err = errors.New("project is required")
		return res
	}
	if b.RemoteURL(ctx) == "" {
		res.Err = errors.New("no shared repository configured")
		return res
	}
	// Before anything touches the clone: settings that cannot be read must
	// stop the publish, not degrade to unscreened.
	set, screened, err := b.settings(ctx)
	var patterns []*regexp.Regexp
	if err == nil && screened {
		patterns, err = set.patterns()
	}
	if err == nil && screened {
		err = b.applyIdentity(ctx, set)
	}
	if err != nil {
		res.Stage, res.Err = "screen", err
		return res
	}

	dirName := ProjectDirName(project)
	projDir := filepath.Join(b.dir, dirName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		res.Stage, res.Err = "export", err
		return res
	}

	// 1. Fast-forward to the remote BEFORE exporting, so the export appends to
	// an up-to-date manifest and no conflict is created in the first place.
	// A repo with no commits has nothing to pull. Neither has a remote with no
	// branch yet — which is not the same thing, and used to be fatal: if the
	// very first publish committed and then failed to push, this clone has a
	// commit while the remote is still empty, and every later publish died
	// here with "couldn't find remote ref master". The recovery path blocked
	// by the damage it exists to recover from.
	if b.git.HasCommits(ctx, b.dir) {
		if r := b.git.PullFFOnly(ctx, b.dir); r.Failed() && !gitcli.IsEmptyRemote(r) {
			res.Stage, res.Output = "pull", r.Output
			res.Err = errors.New("could not fast-forward the shared repo; resolve it before publishing")
			return res
		}
	}
	b.ensureGitAttributes()

	// 2. Export against a PRIVATE view of the project — a temp `.engram/`
	// holding only the chunks this user published — and copy the result back.
	//
	// This indirection is the whole ballgame. engram's file export decides what
	// is "new" by timestamp: it takes the newest created_at in the manifest of
	// the directory it exports into and keeps only rows written after it
	// (sync.go, lastChunkTime + filterNewData). That watermark is a property of
	// the directory, so a shared directory means a shared watermark — and the
	// moment a teammate publishes, everyone else's export silently truncates to
	// "things I wrote since THEIR publish". The first contributor never notices,
	// because an empty manifest exports everything; the second one publishes a
	// chunk with zero memories and no error anywhere. Giving each contributor a
	// manifest containing only their own chunks restores the invariant engram
	// assumes: one writer per `.engram/`.
	//
	// An empty staged view sets the watermark to zero and re-exports the whole
	// project. There is no button for that any more, and there should never
	// have been one: the user cannot see a watermark, so "should I re-export
	// everything?" was never a question they were equipped to answer. It is
	// decided here instead, from the one signal that means our work is
	// genuinely missing — the repo holds no chunk stamped with our name.
	//
	// A clone with no publishing history already stages nothing, so the common
	// first publish is covered without any of this. The escalation catches the
	// leftover: a ledger that claims a history the repository does not
	// corroborate, which is what a renamed user account looks like.
	//
	// Deliberately NOT escalating when our chunks are already there. Then an
	// empty export just means the database has not changed, and re-exporting
	// would duplicate the whole project into the repo to accomplish nothing —
	// the repo grows and no one receives a single observation they lacked.
	staged := b.stagedView(dirName)
	if len(staged) > 0 && len(b.ownChunkIDs(dirName)) == 0 {
		staged = nil
	}

	// An empty staged view means a full re-export — this clone has no
	// publishing history, or the escalation above just decided on one.
	// That is safe only when this clone is up to date: a full export sends our
	// copy of every observation, and the import is last-writer-wins with no
	// timestamp comparison, so anything a teammate corrected while we were
	// behind would be reverted for everyone.
	//
	// The check belongs here rather than in the handler, because this is the
	// first moment the manifest is current — step 1 above just fast-forwarded.
	// A teammate publishing between the panel's render and this call would
	// slip past any earlier check.
	if len(staged) == 0 {
		if behind := b.unappliedCommits(dirName); behind > 0 {
			res.Blocked = true
			res.Err = fmt.Errorf("re-export blocked: %d commit(s) here are not applied on this machine. "+
				"Sending the whole project now would replace other people's newer observations with older copies. Pull %s first",
				behind, project)
			return res
		}
	}

	workDir, err := os.MkdirTemp("", "engram-ui-publish-*")
	if err != nil {
		res.Stage, res.Err = "export", fmt.Errorf("create temp dir: %w", err)
		return res
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	if err := b.stageSubset(workDir, dirName, staged, true); err != nil {
		res.Stage, res.Err = "export", err
		return res
	}

	exp := b.cli.LocalExport(ctx, workDir, project, set.CreatedBy)
	res.Output = stripGitHint(exp.Output)
	if exp.Failed() {
		res.Stage, res.Err = "export", exp.Err
		return res
	}

	exported := filepath.Join(workDir, syncDirName, chunksDirName)
	added, err := newChunks(workDir, staged)
	if err != nil {
		res.Stage, res.Err = "export", err
		return res
	}
	if screened && len(added) > 0 {
		findings, err := screenChunks(exported, project, added, patterns)
		if err == nil && len(findings) > 0 {
			err = b.quarantine(exported, dirName, added)
		}
		if err != nil {
			res.Stage, res.Err = "screen", err
			return res
		}
		if len(findings) > 0 {
			res.Blocked, res.Findings = true, findings
			res.Err = fmt.Errorf("publish blocked — %d observation(s) failed this repository's screening; nothing was published", len(findings))
			return res
		}
	}
	if err := b.copyIn(exported, dirName, added); err != nil {
		res.Stage, res.Err = "export", err
		return res
	}
	// A new chunk covers everything a quarantined one did: same watermark,
	// fresher content.
	if len(added) > 0 {
		b.clearQuarantine(dirName)
	}
	// Record what we just wrote: it came out of this database, so it is not
	// something to pull back, and it is the watermark our next export stages.
	b.markPublished(dirName, chunkIDs(added))
	res.Exported = len(added) > 0
	if !res.Exported {
		res.Warn = b.diagnoseEmptyExport(dirName)

		// A zero-watermark export that produced nothing is engram stating that
		// this database has nothing left to export — everything it holds is
		// already in the repo. Adopt the repo's own chunks as this machine's
		// publishing history, otherwise the panel warns "no record of
		// publishing" forever: it tells you to publish, the publish correctly
		// does nothing, nothing gets recorded, and the warning comes back.
		//
		// Claiming chunks by username is exactly what stagedView refuses to do
		// — normally it would hand another machine's timestamp to our
		// watermark and truncate our next export. It is safe in this one spot
		// and nowhere else: engram has just told us there is nothing left to
		// truncate.
		if len(staged) == 0 {
			b.markPublished(dirName, b.ownChunkIDs(dirName))
		}
	}
	return b.commitAndPush(ctx, res, dirName, project)
}

// PublishQuarantined publishes the chunk the screening refused, once the user
// has judged its findings a false alarm. publish minus the export: the content
// is the quarantined chunk exactly as it was screened.
func (b *Bridge) PublishQuarantined(ctx context.Context, project string) PushResult {
	b.mu.Lock()
	defer b.mu.Unlock()

	res := PushResult{Project: project}
	dirName := ProjectDirName(project)
	held := b.quarantineDir(dirName)
	m, err := readManifest(filepath.Join(held, manifestName))
	if strings.TrimSpace(project) == "" || err != nil || len(m.Chunks) == 0 {
		res.Err = fmt.Errorf("no held-back commit to publish for %q", project)
		return res
	}
	if b.RemoteURL(ctx) == "" {
		res.Err = errors.New("no shared repository configured")
		return res
	}
	set, found, err := b.settings(ctx)
	if err == nil && found {
		err = b.applyIdentity(ctx, set)
	}
	if err != nil {
		res.Stage, res.Err = "screen", err
		return res
	}
	// Same unconditional fast-forward as publish, for the same reason.
	if b.git.HasCommits(ctx, b.dir) {
		if r := b.git.PullFFOnly(ctx, b.dir); r.Failed() && !gitcli.IsEmptyRemote(r) {
			res.Stage, res.Output = "pull", r.Output
			res.Err = errors.New("could not fast-forward the shared repo; resolve it before publishing")
			return res
		}
	}
	b.ensureGitAttributes()
	if err := b.copyIn(filepath.Join(held, chunksDirName), dirName, m.Chunks); err != nil {
		res.Stage, res.Err = "export", err
		return res
	}
	b.markPublished(dirName, chunkIDs(m.Chunks))
	b.clearQuarantine(dirName)
	res.Exported = true
	return b.commitAndPush(ctx, res, dirName, project)
}

// commitAndPush is the tail both publishes share: commit what landed in the
// project directory, then push, reconciling concurrent publishes.
func (b *Bridge) commitAndPush(ctx context.Context, res PushResult, dirName, project string) PushResult {
	// 3. Commit whatever the export produced, plus .gitattributes on first use.
	tracked := []string{dirName, gitAttributesName}
	changed, err := b.git.HasStagedOrUntracked(ctx, b.dir, tracked...)
	if err != nil {
		res.Stage, res.Err = "commit", err
		return res
	}
	// Nothing new to commit is normally the end of it — but not if an earlier
	// publish committed and then failed to push. That work is on this machine
	// only, the export will never produce it again (it is already committed),
	// and without this check every later publish reports "nothing new to
	// publish" while it sits here forever. Fall through to the push step.
	if !changed && !b.git.HasUnpushedCommits(ctx, b.dir) {
		return res // already published; not an error
	}
	if changed {
		if r := b.git.Add(ctx, b.dir, tracked...); r.Failed() {
			res.Stage, res.Output, res.Err = "commit", joinOutput(res.Output, r.Output), r.Err
			return res
		}
		commit := b.git.Commit(ctx, b.dir, "engram: publish "+project)
		if commit.Failed() && !gitcli.IsNothingToCommit(commit) {
			res.Stage, res.Output, res.Err = "commit", joinOutput(res.Output, commit.Output), commit.Err
			return res
		}
	}

	// 4. Push, reconciling concurrent publishes by merging manifests.
	for attempt := 1; ; attempt++ {
		push := b.git.Push(ctx, b.dir)
		if !push.Failed() {
			res.Pushed = true
			return res
		}
		if !gitcli.IsRejected(push) || attempt >= maxPushAttempts {
			res.Stage, res.Output, res.Err = "push", joinOutput(res.Output, push.Output), push.Err
			return res
		}
		if err := b.reconcile(ctx); err != nil {
			res.Stage, res.Output, res.Err = "push", joinOutput(res.Output, push.Output), err
			return res
		}
	}
}

// stagedView returns the manifest entries this export should see: the chunks
// THIS CLONE published, and nothing else. Every foreign entry silently raises
// engram's "what counts as new" watermark, so the rule is strict.
//
// Deliberately not "chunks with my username". engram attributes a chunk to a
// user, not to a machine, so the same person on a laptop and a workstation
// produces chunks under one name — and staging the laptop's would truncate the
// workstation's export exactly like a teammate's did. The ledger lives inside
// .git/ of this clone, so it can tell the two apart when created_by cannot.
//
// A clone with no publishing history stages nothing, which sets the watermark
// to zero and exports the project whole. That is right for a first publish and
// merely wasteful for a clone that lost its ledger: the import deduplicates, so
// the cost is repository size, never correctness. Note this makes the first
// publish after upgrading a full re-export, once.
func (b *Bridge) stagedView(dirName string) []ChunkEntry {
	mine := b.publishedFrom(dirName)
	if len(mine) == 0 {
		return nil
	}
	m, err := readManifest(b.manifestPath(dirName))
	if err != nil {
		return nil
	}
	out := make([]ChunkEntry, 0, len(mine))
	for _, c := range m.Chunks {
		if mine[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// adoptChunks moves what the staged export produced into the shared repo: the
// new chunk files get copied in, and their manifest entries are unioned into
// the project's real manifest. Returns the entries that were added.
//
// The repo's manifest is never handed to engram and never overwritten with a
// staged one — it is only ever grown. That is the same rule the merge driver
// follows, and for the same reason: an entry dropped from the manifest is a
// chunk nobody will ever import again.
func (b *Bridge) adoptChunks(workDir, dirName string, staged []ChunkEntry) ([]ChunkEntry, error) {
	added, err := newChunks(workDir, staged)
	if err != nil {
		return nil, err
	}
	if err := b.copyIn(filepath.Join(workDir, syncDirName, chunksDirName), dirName, added); err != nil {
		return nil, err
	}
	return added, nil
}

// newChunks returns what the export in workDir produced beyond the staged view.
func newChunks(workDir string, staged []ChunkEntry) ([]ChunkEntry, error) {
	produced, err := readManifest(filepath.Join(workDir, syncDirName, manifestName))
	if err != nil {
		return nil, fmt.Errorf("read exported manifest: %w", err)
	}
	was := make(map[string]bool, len(staged))
	for _, e := range staged {
		was[e.ID] = true
	}
	var added []ChunkEntry
	for _, e := range produced.Chunks {
		if !was[e.ID] {
			added = append(added, e)
		}
	}
	return added, nil
}

// copyIn copies chunks from srcChunks into the project and unions their entries
// into its manifest.
func (b *Bridge) copyIn(srcChunks, dirName string, added []ChunkEntry) error {
	if len(added) == 0 {
		return nil
	}
	if err := copyChunkFiles(srcChunks, filepath.Join(b.dir, dirName, syncDirName, chunksDirName), added); err != nil {
		return err
	}

	// Union rather than append: the pull may have brought in entries we did not
	// stage, and UnionManifests already owns the dedupe + ordering rules.
	current, err := os.ReadFile(b.manifestPath(dirName))
	if err != nil {
		current = []byte(`{"version":1,"chunks":[]}`) // first publish of this project
	}
	mine, err := marshalManifest(Manifest{Version: 1, Chunks: added})
	if err != nil {
		return err
	}
	merged, err := UnionManifests(current, mine)
	if err != nil {
		return fmt.Errorf("merge exported manifest: %w", err)
	}
	if err := os.WriteFile(b.manifestPath(dirName), merged, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

func copyChunkFiles(src, dst string, chunks []ChunkEntry) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create chunks dir: %w", err)
	}
	for _, e := range chunks {
		name := e.ID + ".jsonl.gz"
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			return fmt.Errorf("read exported chunk %s: %w", e.ID, err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o644); err != nil {
			return fmt.Errorf("write chunk %s: %w", e.ID, err)
		}
	}
	return nil
}

// reconcile merges the remote after a rejected push. Chunk files never conflict
// (each is a new, content-addressed path), so the only expected conflict is the
// manifest, and the only correct resolution is the union of both sides. Any
// other conflicted path means something we don't understand is in the repo, so
// the merge is aborted rather than guessed at.
func (b *Bridge) reconcile(ctx context.Context) error {
	if r := b.git.Fetch(ctx, b.dir); r.Failed() {
		return fmt.Errorf("fetch failed: %s", firstLine(r.Output))
	}
	merge := b.git.Merge(ctx, b.dir, "origin/"+b.git.Branch(ctx, b.dir))
	if !merge.Failed() {
		return nil
	}

	conflicts, err := b.git.Unmerged(ctx, b.dir)
	if err != nil || len(conflicts) == 0 {
		_ = b.git.MergeAbort(ctx, b.dir)
		return fmt.Errorf("merge failed: %s", firstLine(merge.Output))
	}
	for _, f := range conflicts {
		if !isManifestPath(f) {
			_ = b.git.MergeAbort(ctx, b.dir)
			return fmt.Errorf("unexpected conflict in %s — resolve the shared repo by hand", f)
		}
	}

	for _, f := range conflicts {
		ours, err := b.git.ShowStage(ctx, b.dir, 2, f)
		if err != nil {
			_ = b.git.MergeAbort(ctx, b.dir)
			return err
		}
		theirs, err := b.git.ShowStage(ctx, b.dir, 3, f)
		if err != nil {
			_ = b.git.MergeAbort(ctx, b.dir)
			return err
		}
		merged, err := UnionManifests(ours, theirs)
		if err != nil {
			_ = b.git.MergeAbort(ctx, b.dir)
			return fmt.Errorf("merge %s: %w", f, err)
		}
		if err := os.WriteFile(filepath.Join(b.dir, filepath.FromSlash(f)), merged, 0o644); err != nil {
			_ = b.git.MergeAbort(ctx, b.dir)
			return err
		}
		if r := b.git.Add(ctx, b.dir, f); r.Failed() {
			_ = b.git.MergeAbort(ctx, b.dir)
			return r.Err
		}
	}
	if r := b.git.CommitMerge(ctx, b.dir); r.Failed() {
		_ = b.git.MergeAbort(ctx, b.dir)
		return fmt.Errorf("merge commit failed: %s", firstLine(r.Output))
	}
	return nil
}

// ── retrieve ──────────────────────────────────────────────────────────────

// PullResult is the outcome of a retrieve, shaped for the UI banner.
type PullResult struct {
	Project       string
	CreatedBy     string // "" = every contributor
	ChunksMatched int
	ChunksTotal   int
	Output        string
	Err           error
}

// Pull imports every chunk of one project from the shared repo.
func (b *Bridge) Pull(ctx context.Context, project string) PullResult {
	b.mu.Lock()
	defer b.mu.Unlock()

	res := PullResult{Project: project}
	dirName := ProjectDirName(project)
	if err := b.refresh(ctx); err != nil {
		res.Err = err
		return res
	}
	projDir := filepath.Join(b.dir, dirName)
	m, err := readManifest(b.manifestPath(dirName))
	if err != nil {
		res.Err = fmt.Errorf("%q is not in the shared repo yet", project)
		return res
	}
	res.ChunksTotal, res.ChunksMatched = len(m.Chunks), len(m.Chunks)

	imp := b.cli.LocalImport(ctx, projDir)
	res.Output, res.Err = imp.Output, imp.Err
	if res.Err == nil {
		b.markImported(dirName, chunkIDs(m.Chunks))
	}
	return res
}

// PullSelective imports only the chunks a chosen contributor published.
//
// The filter is per chunk, and created_by is whoever ran the export, not the
// author of each memory. So a correction one person made to another's memory
// lives in the corrector's chunk: filtering to a single contributor can leave a
// stale copy behind. Good for "bring me what Ana shared", not for maintaining a
// permanent partial mirror.
func (b *Bridge) PullSelective(ctx context.Context, project, createdBy string) PullResult {
	b.mu.Lock()
	defer b.mu.Unlock()

	res := PullResult{Project: project, CreatedBy: createdBy}
	dirName := ProjectDirName(project)
	if err := b.refresh(ctx); err != nil {
		res.Err = err
		return res
	}
	m, err := readManifest(b.manifestPath(dirName))
	if err != nil {
		res.Err = fmt.Errorf("%q is not in the shared repo yet", project)
		return res
	}
	res.ChunksTotal = len(m.Chunks)

	// By committer, because that is the name the contributor list offered.
	// The rewritten created_by then lands in the staged manifest, which is
	// harmless: engram's import reads only the chunk id from it.
	entries := FilterChunks(attributed(dirName, m, b.committers(ctx)).Chunks, createdBy)
	res.ChunksMatched = len(entries)
	if len(entries) == 0 {
		return res // nothing matched; don't spin up a temp dir or exec
	}

	workDir, err := os.MkdirTemp("", "engram-ui-share-*")
	if err != nil {
		res.Err = fmt.Errorf("create temp dir: %w", err)
		return res
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	if err := b.stageSubset(workDir, dirName, entries, false); err != nil {
		res.Err = err
		return res
	}
	imp := b.cli.LocalImport(ctx, workDir)
	res.Output, res.Err = imp.Output, imp.Err
	if res.Err == nil {
		b.markImported(dirName, chunkIDs(entries))
	}
	return res
}

// stageSubset builds a throwaway `.engram/` holding a filtered manifest and the
// selected chunks. The chunk files are copied byte for byte straight out of the
// clone: they are already gzip in exactly the layout engram's reader expects,
// so decompressing and recompressing would be pure waste (and would risk
// producing a different encoding of the same data).
//
// skipMissing decides what a chunk file that isn't on disk means. Importing
// (skipMissing=false): a fault — the whole point is to apply those chunks, so a
// gap must be loud. Publishing (skipMissing=true): survivable — the manifest
// entry still carries the timestamp that sets the watermark, and engram itself
// treats an unreadable chunk as "contributes no known relations, at worst one
// gets re-exported, which is an idempotent upsert". Refusing to publish because
// a chunk of ours went missing from the clone would be the harsher answer.
func (b *Bridge) stageSubset(workDir, dirName string, entries []ChunkEntry, skipMissing bool) error {
	syncDir := filepath.Join(workDir, syncDirName)
	chunksDir := filepath.Join(syncDir, chunksDirName)
	if err := os.MkdirAll(chunksDir, 0o755); err != nil {
		return fmt.Errorf("create chunks dir: %w", err)
	}
	body, err := marshalManifest(Manifest{Version: 1, Chunks: entries})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(syncDir, manifestName), body, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	for _, e := range entries {
		name := e.ID + ".jsonl.gz"
		src := filepath.Join(b.dir, dirName, syncDirName, chunksDirName, name)
		data, err := os.ReadFile(src)
		if err != nil {
			if skipMissing && os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read chunk %s: %w", e.ID, err)
		}
		if err := os.WriteFile(filepath.Join(chunksDir, name), data, 0o644); err != nil {
			return fmt.Errorf("write chunk %s: %w", e.ID, err)
		}
	}
	return nil
}

// refresh fast-forwards the clone before a read.
func (b *Bridge) refresh(ctx context.Context) error {
	if b.RemoteURL(ctx) == "" {
		return errors.New("no shared repository configured")
	}
	if !b.git.HasCommits(ctx, b.dir) {
		return errors.New("the shared repo is empty — nobody has published yet")
	}
	if r := b.git.PullFFOnly(ctx, b.dir); r.Failed() {
		return fmt.Errorf("could not update the shared repo: %s", firstLine(r.Output))
	}
	return nil
}

// ── pure helpers ──────────────────────────────────────────────────────────

// UnionManifests merges two sides of a conflicted manifest by chunk id.
//
// Concurrent publishes each append an entry to the same JSON array, which git
// cannot auto-merge: the conflict markers land inside an object because the
// trailing fields of both entries are identical. A union merge driver would
// emit invalid JSON for the same reason. Since chunks are immutable and
// content-addressed, "both, deduplicated by id" is not a heuristic — it is the
// only correct answer, and it is what the remote would have held if the two
// publishes had been sequential.
func UnionManifests(ours, theirs []byte) ([]byte, error) {
	a, err := parseManifest(ours)
	if err != nil {
		return nil, fmt.Errorf("parse ours: %w", err)
	}
	bb, err := parseManifest(theirs)
	if err != nil {
		return nil, fmt.Errorf("parse theirs: %w", err)
	}

	seen := make(map[string]bool, len(a.Chunks)+len(bb.Chunks))
	merged := make([]ChunkEntry, 0, len(a.Chunks)+len(bb.Chunks))
	for _, e := range append(append([]ChunkEntry{}, a.Chunks...), bb.Chunks...) {
		if e.ID == "" || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		merged = append(merged, e)
	}
	// created_at is RFC3339 UTC, so lexical order is chronological. The id
	// tie-break keeps the result identical on every machine that merges.
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].CreatedAt != merged[j].CreatedAt {
			return merged[i].CreatedAt < merged[j].CreatedAt
		}
		return merged[i].ID < merged[j].ID
	})

	version := a.Version
	if version == 0 {
		version = bb.Version
	}
	if version == 0 {
		version = 1
	}
	return marshalManifest(Manifest{Version: version, Chunks: merged})
}

// FilterChunks returns the entries a contributor published. An empty createdBy
// means "everyone". The unknownUser sentinel matches entries with no author.
func FilterChunks(entries []ChunkEntry, createdBy string) []ChunkEntry {
	if strings.TrimSpace(createdBy) == "" {
		return entries
	}
	out := make([]ChunkEntry, 0, len(entries))
	for _, c := range entries {
		if authorOf(c) == createdBy {
			out = append(out, c)
		}
	}
	return out
}

// silentContributors names the people who have published to a project and
// never landed a single memory in it.
//
// This one reads the manifest's counters, not the chunks, even though every
// other number on the panel now comes from the chunks. The counter is what the
// publisher's own engram wrote down about its export — "this chunk carried zero
// memories" — which is precisely the claim being checked, and it survives a
// chunk file that never made it into the clone. Counting the file instead would
// turn every missing chunk into an accusation.
//
// That combination is a fingerprint, not a preference. Publishing carries
// memories; the two ways to publish none of them are an engram-ui old enough to
// still export into the shared directory (where a teammate's timestamp
// truncated the export to nothing) and an engram that already exported this
// project to some other `.engram/`. Both are silent on the publisher's side —
// they see a successful push — so the only place the failure can surface is
// here, in front of the people waiting for those memories.
//
// Deliberately "never any", not "none this time": once someone has landed
// memories, a chunk carrying only relation judgements is a perfectly ordinary
// publish and must not be flagged.
func silentContributors(m Manifest) []string {
	claimed := map[string]int{}
	for _, c := range m.Chunks {
		claimed[authorOf(c)] += c.Memories
	}
	var out []string
	for user, n := range claimed {
		if n == 0 {
			out = append(out, user)
		}
	}
	sort.Strings(out)
	return out
}

// ProjectDirName maps a project name to a directory name safe on every
// filesystem. The mapping only has to be deterministic, never reversible: the
// directory is an organisational label, while the project each memory belongs
// to is carried inside the chunks themselves, and publishing always passes the
// real name to engram explicitly.
//
// The result is lower-cased, and that is a correctness requirement rather than
// a style choice. engram derives a project name from whatever directory or repo
// the user happens to have checked out, so "infinity-core", "Infinity-core" and
// "INFINITY-CORE" all reach us for the same project. Git's index is
// case-sensitive while Windows and macOS filesystems are not, so publishing
// those as distinct folders would put entries in the tree that cannot coexist
// in one working directory: everyone on a case-insensitive machine gets a
// broken checkout of the shared repo. Folding the case means two spellings
// converge on one folder, which is also what a human would expect.
//
// Consequence worth knowing: two genuinely different projects distinguished
// only by case would share a folder. A case-insensitive filesystem could not
// hold both anyway, so converging is the only workable answer.
//
// Must stay idempotent — discovery feeds directory names straight back in.
func ProjectDirName(project string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(project) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "project"
	}
	return name
}

func authorOf(c ChunkEntry) string {
	if strings.TrimSpace(c.CreatedBy) == "" {
		return unknownUser
	}
	return c.CreatedBy
}

func isManifestPath(p string) bool {
	return strings.HasSuffix(p, syncDirName+"/"+manifestName)
}

func (b *Bridge) manifestPath(dirName string) string {
	return filepath.Join(b.dir, dirName, syncDirName, manifestName)
}

// diagnoseEmptyExport explains an export that produced nothing.
//
// Usually it means exactly what it says: nothing has changed since this
// machine's last publish. The interesting case is a project that has never been
// published from here and still exports nothing, which points at engram's
// chunk-content bookkeeping — it records every chunk it exports under a single
// global "local" target, so a project already exported to some other `.engram/`
// hashes to a chunk it has seen and gets skipped. A full re-export is the way
// out, unless the content is byte-identical to that earlier export, in which
// case the old directory has to be copied in to restore continuity.
// One message, not two: by the time an export comes back empty with none of
// our chunks in the repo, publish has already escalated to a full re-export
// and that produced nothing either. There is no second thing left to try, so
// there is no longer a variant that suggests one.
//
// Wording note: this lands on the shared-repo screen, which calls a chunk a
// "commit" and a memory an "observation". See share.html.tmpl.
func (b *Bridge) diagnoseEmptyExport(dirName string) string {
	// Chunks of ours already in the repo means nothing is lost: the export
	// found nothing to add because the database has not changed since the last
	// one. Warning here would send someone repairing what is already fine.
	if len(b.ownChunkIDs(dirName)) > 0 {
		return ""
	}
	return "Nothing was exported, and none of your observations are in the repository. " +
		"engram never re-exports content whose hash it has already recorded, so this " +
		"project was exported to some other .engram/ directory in the past — copy that " +
		"directory's files in here to restore continuity."
}

// ownChunkIDs lists the repo's chunks for a project that carry the local
// username. Only ever used to close the empty-export loop in publish; see the
// note there for why attributing by username is safe in that one case.
func (b *Bridge) ownChunkIDs(dirName string) []string {
	m, err := readManifest(b.manifestPath(dirName))
	if err != nil {
		return nil
	}
	return chunkIDs(FilterChunks(m.Chunks, b.user()))
}

// ensureGitAttributes writes .gitattributes on first publish. Best-effort: a
// missing file only risks cosmetic line-ending churn on the manifest.
func (b *Bridge) ensureGitAttributes() {
	path := filepath.Join(b.dir, gitAttributesName)
	if _, err := os.Stat(path); err == nil {
		return
	}
	_ = os.WriteFile(path, []byte(gitAttributes), 0o644)
}

func readManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	return parseManifest(data)
}

func parseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// marshalManifest encodes exactly as engram does — two-space indent, no
// trailing newline — so our merge commits don't churn a line against every
// manifest engram writes itself.
func marshalManifest(m Manifest) ([]byte, error) {
	if m.Chunks == nil {
		m.Chunks = []ChunkEntry{}
	}
	return json.MarshalIndent(m, "", "  ")
}

// stripGitHint drops the tail engram prints after a file-mode export:
//
//	Add to git:
//	  git add .engram/ && git commit -m "sync engram memories"
//
// Sound advice for someone running the CLI by hand, wrong here: the very next
// thing publish() does is add, commit and push. Leaving it in the panel tells
// the user to redo work that is already done.
func stripGitHint(out string) string {
	i := strings.Index(out, "Add to git:")
	if i < 0 {
		return out
	}
	return strings.TrimRight(out[:i], " \r\n")
}

func joinOutput(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
