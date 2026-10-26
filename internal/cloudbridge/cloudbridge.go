// Package cloudbridge implements selective cloud pull: download a chosen subset
// of a project's chunks from the engram cloud, then apply them via a LOCAL
// import — bypassing engram's all-or-nothing cloud import.
//
// How it works (no engram changes needed):
//  1. Read the cloud chunk manifest over HTTP (cloudclient).
//  2. Filter the manifest entries by created_by (the pusher) — empty = all.
//  3. Write a temp `.engram/` layout: manifest.json (filtered) + gzipped chunks.
//  4. Exec `engram sync --import` (local mode) with CWD = the temp dir, which
//     applies ONLY the chunks present (engramcli.LocalImport).
//
// The gzip format mirrors engram's FileTransport.WriteChunk exactly so the
// local import's readGzip can decompress it. The local import tracks applied
// chunks under engram's global "local" key, so repeat pulls dedup correctly;
// observation apply is idempotent by sync_id, so overlap with the normal cloud
// pull path is harmless.
package cloudbridge

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edalandete/engram-ui/internal/cloudclient"
	"github.com/edalandete/engram-ui/internal/engramcli"
)

// unknownUser is the bucket label for chunks with an empty created_by.
const unknownUser = "(unknown)"

// Bridge orchestrates selective pull. Stateless: the cloud client (which
// depends on the per-request server URL + token) is passed into Pull.
type Bridge struct {
	cli *engramcli.Runner
}

func New(cli *engramcli.Runner) *Bridge { return &Bridge{cli: cli} }

// UserCount aggregates a manifest by pusher for the UI dropdown.
//
// Obs is the count of observations FIRST CONTRIBUTED by the user — each
// observation (by sync_id) is credited to exactly one pusher: the earliest one
// to push it (manifest order = created_at ASC). This makes the per-pusher Obs
// counts disjoint, so they SUM to the project's true unique-observation total.
// It is populated only by UsersWithObsCounts (which downloads chunk payloads);
// the pure Users() leaves it 0 because the manifest alone has no sync_ids.
type UserCount struct {
	User   string
	Chunks int
	Obs    int
}

// Users returns distinct created_by values with chunk counts, sorted by user.
// Empty created_by is bucketed as "(unknown)". Pure — does NOT set Obs (the
// manifest only carries per-chunk sums that overcount; use UsersWithObsCounts
// for the real unique-observation count).
func Users(m *cloudclient.Manifest) []UserCount {
	if m == nil {
		return nil
	}
	idx := map[string]*UserCount{}
	order := []string{}
	for _, c := range m.Chunks {
		key := c.CreatedBy
		if strings.TrimSpace(key) == "" {
			key = unknownUser
		}
		uc, ok := idx[key]
		if !ok {
			uc = &UserCount{User: key}
			idx[key] = uc
			order = append(order, key)
		}
		uc.Chunks++
	}
	sort.Strings(order)
	out := make([]UserCount, 0, len(order))
	for _, k := range order {
		out = append(out, *idx[k])
	}
	return out
}

// FilterChunks returns entries whose created_by matches createdBy. An empty
// createdBy returns all entries (the "Pull total" case). The sentinel
// "(unknown)" matches entries with an empty created_by. Pure.
func FilterChunks(entries []cloudclient.ChunkEntry, createdBy string) []cloudclient.ChunkEntry {
	if strings.TrimSpace(createdBy) == "" {
		return entries
	}
	out := make([]cloudclient.ChunkEntry, 0, len(entries))
	for _, c := range entries {
		cb := c.CreatedBy
		if strings.TrimSpace(cb) == "" {
			cb = unknownUser
		}
		if cb == createdBy {
			out = append(out, c)
		}
	}
	return out
}

// PullResult is the outcome of a selective pull, shaped for the UI banner.
type PullResult struct {
	Project       string
	CreatedBy     string // "" = all users
	ChunksMatched int
	ChunksTotal   int
	Output        string // engram local-import stdout/stderr
	Err           error
}

// Pull performs a selective pull. createdBy="" pulls all chunks for the project.
// cc is a cloud client already bound to the cloud server + token.
func (b *Bridge) Pull(ctx context.Context, cc *cloudclient.Client, project, createdBy string) PullResult {
	res := PullResult{Project: project, CreatedBy: createdBy}

	manifest, err := cc.ReadManifest(ctx, project)
	if err != nil {
		res.Err = err
		return res
	}
	res.ChunksTotal = len(manifest.Chunks)

	entries := FilterChunks(manifest.Chunks, createdBy)
	res.ChunksMatched = len(entries)
	if len(entries) == 0 {
		// Nothing to do — don't spin up a temp dir or exec.
		return res
	}

	workDir, err := os.MkdirTemp("", "engram-ui-pull-*")
	if err != nil {
		res.Err = fmt.Errorf("create temp dir: %w", err)
		return res
	}
	defer os.RemoveAll(workDir)

	fetch := func(id string) ([]byte, error) { return cc.ReadChunk(ctx, project, id) }
	if err := writeSyncDir(workDir, entries, fetch); err != nil {
		res.Err = err
		return res
	}

	importRes := b.cli.LocalImport(ctx, workDir)
	res.Output = importRes.Output
	res.Err = importRes.Err
	return res
}

// BuildAuthorIndex returns a map sync_id → pusher (created_by) for every
// observation present in the project's cloud chunks. engram stores created_by
// only at chunk level (who pushed), not per observation, so we derive the map:
// read the manifest, then open each chunk and attribute its observations to the
// chunk's created_by. Used to show provenance ("from cloud · pushed by X") on
// observations that originated from the cloud. Observations never pushed to the
// cloud simply won't appear in the map (rendered without a badge = local/mine).
//
// If the same sync_id appears in multiple chunks (e.g. a re-push by a different
// user), the LAST chunk in manifest order wins — manifest is ordered by
// created_at ASC, so this reflects the most recent pusher.
func (b *Bridge) BuildAuthorIndex(ctx context.Context, cc *cloudclient.Client, project string) (map[string]string, error) {
	manifest, err := cc.ReadManifest(ctx, project)
	if err != nil {
		return nil, err
	}
	index := make(map[string]string)
	for _, entry := range manifest.Chunks {
		data, err := cc.ReadChunk(ctx, project, entry.ID)
		if err != nil {
			return nil, fmt.Errorf("read chunk %s: %w", entry.ID, err)
		}
		pusher := entry.CreatedBy
		if strings.TrimSpace(pusher) == "" {
			pusher = unknownUser
		}
		for _, syncID := range observationSyncIDs(data) {
			index[syncID] = pusher
		}
	}
	return index, nil
}

// UsersWithObsCounts returns per-pusher chunk counts plus the number of
// observations each pusher FIRST CONTRIBUTED. It downloads every chunk payload
// and attributes each sync_id to exactly ONE pusher: the earliest to push it
// (manifest order is created_at ASC). Because engram's push is incremental but a
// reset+repush (or a server reprovision) can re-upload another user's chunks
// under a new created_by, the same sync_id can appear in multiple pushers'
// chunks. Crediting only the first push keeps the per-pusher counts disjoint, so
// they SUM to the project's true unique-observation total — matching the local
// observation count, not an inflated sum.
func (b *Bridge) UsersWithObsCounts(ctx context.Context, cc *cloudclient.Client, project string) ([]UserCount, error) {
	manifest, err := cc.ReadManifest(ctx, project)
	if err != nil {
		return nil, err
	}
	type agg struct {
		chunks int
		obs    int
	}
	idx := map[string]*agg{}
	order := []string{}
	seen := map[string]struct{}{} // sync_ids already credited to an earlier pusher
	for _, entry := range manifest.Chunks {
		key := entry.CreatedBy
		if strings.TrimSpace(key) == "" {
			key = unknownUser
		}
		a, ok := idx[key]
		if !ok {
			a = &agg{}
			idx[key] = a
			order = append(order, key)
		}
		a.chunks++
		data, err := cc.ReadChunk(ctx, project, entry.ID)
		if err != nil {
			return nil, fmt.Errorf("read chunk %s: %w", entry.ID, err)
		}
		for _, syncID := range observationSyncIDs(data) {
			if _, dup := seen[syncID]; dup {
				continue // already credited to whoever pushed it first
			}
			seen[syncID] = struct{}{}
			a.obs++
		}
	}
	sort.Strings(order)
	out := make([]UserCount, 0, len(order))
	for _, k := range order {
		out = append(out, UserCount{User: k, Chunks: idx[k].chunks, Obs: idx[k].obs})
	}
	return out, nil
}

// observationSyncIDs extracts the sync_id of each observation in a chunk's raw
// JSON payload. Tolerant: ignores entries without a sync_id.
func observationSyncIDs(chunkJSON []byte) []string {
	var chunk struct {
		Observations []struct {
			SyncID string `json:"sync_id"`
		} `json:"observations"`
	}
	if err := json.Unmarshal(chunkJSON, &chunk); err != nil {
		return nil
	}
	out := make([]string, 0, len(chunk.Observations))
	for _, o := range chunk.Observations {
		if s := strings.TrimSpace(o.SyncID); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// writeSyncDir builds the `.engram/` layout under workDir: a filtered
// manifest.json and one gzipped chunk per entry. Aborts on the first fetch or
// write error so a partial directory is never handed to the import.
func writeSyncDir(workDir string, entries []cloudclient.ChunkEntry, fetch func(id string) ([]byte, error)) error {
	syncDir := filepath.Join(workDir, ".engram")
	chunksDir := filepath.Join(syncDir, "chunks")
	if err := os.MkdirAll(chunksDir, 0o755); err != nil {
		return fmt.Errorf("create chunks dir: %w", err)
	}

	manifest := cloudclient.Manifest{Version: 1, Chunks: entries}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(syncDir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	for _, entry := range entries {
		data, err := fetch(entry.ID)
		if err != nil {
			return fmt.Errorf("fetch chunk %s: %w", entry.ID, err)
		}
		if err := writeGzipChunk(filepath.Join(chunksDir, entry.ID+".jsonl.gz"), data); err != nil {
			return fmt.Errorf("write chunk %s: %w", entry.ID, err)
		}
	}
	return nil
}

// writeGzipChunk gzips raw bytes to path, mirroring engram's writeGzip
// (internal/sync/sync.go) so the local import's readGzip can decompress it.
func writeGzipChunk(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	if _, err := gz.Write(data); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
}
