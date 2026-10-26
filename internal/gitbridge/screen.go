package gitbridge

// Screening: what a chunk must not carry out of this machine.
//
// Runs only for repositories with settings (settings.go), between the export
// and the copy into the clone, so a refused chunk never touches the repository.
// Each session, observation and prompt in the chunk must belong to the project
// being published, must not be personal scope, and no string in it may match
// the repository's blocklist.
//
// A refused chunk is quarantined in <clone>/.git/engram-ui-quarantine/<project>/
// instead of discarded. engram never exports the same content twice — it
// records the hash of every chunk it writes — so if the user judges a finding a
// false alarm, re-exporting would produce nothing and that content could never
// be published. Inside .git because it belongs to this clone and must never be
// committed; Disconnect throwing it away only costs the "publish anyway" button.

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const quarantineDirName = "engram-ui-quarantine"

// Finding is one item the screening refused.
type Finding struct {
	ID      string // sync_id, else the item's own id
	Title   string
	Reason  string
	Excerpt string // the matched text in context, for blocklist findings
}

// screenChunks screens the given chunks of dir (a chunks directory).
func screenChunks(dir, project string, chunks []ChunkEntry, patterns []*regexp.Regexp) ([]Finding, error) {
	var out []Finding
	for _, c := range chunks {
		found, err := screenChunk(filepath.Join(dir, c.ID+".jsonl.gz"), project, patterns)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

func screenChunk(path, project string, patterns []*regexp.Regexp) ([]Finding, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("read chunk %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = gz.Close() }()

	var doc map[string]json.RawMessage
	if err := json.NewDecoder(gz).Decode(&doc); err != nil {
		return nil, fmt.Errorf("read chunk %s: %w", filepath.Base(path), err)
	}
	var out []Finding
	for _, kind := range []string{"sessions", "observations", "prompts"} {
		raw := doc[kind]
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		// Anything but a list is a shape this code does not understand, and
		// skipping it would publish it unscreened.
		var items []map[string]any
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("chunk %s: unexpected %s: %w", filepath.Base(path), kind, err)
		}
		for _, item := range items {
			if f, bad := screenItem(item, project, patterns); bad {
				out = append(out, f)
			}
		}
	}
	return out, nil
}

func screenItem(item map[string]any, project string, patterns []*regexp.Regexp) (Finding, bool) {
	f := Finding{ID: str(item["sync_id"]), Title: str(item["title"])}
	if f.ID == "" {
		f.ID = str(item["id"])
	}
	if p := str(item["project"]); p != "" && !strings.EqualFold(p, project) {
		f.Reason = "belongs to project " + p
		return f, true
	}
	if str(item["scope"]) == "personal" {
		f.Reason = "personal scope"
		return f, true
	}
	text := strings.Join(stringsIn(item), "\n")
	for _, re := range patterns {
		if loc := re.FindStringIndex(text); loc != nil {
			f.Reason = "matches " + strings.TrimPrefix(re.String(), "(?i)")
			f.Excerpt = excerpt(text, loc)
			return f, true
		}
	}
	return f, false
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// stringsIn collects every string value in v, keys sorted so findings are
// stable.
func stringsIn(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, stringsIn(e)...)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []string
		for _, k := range keys {
			out = append(out, stringsIn(t[k])...)
		}
		return out
	}
	return nil
}

// excerpt returns the match with up to 40 bytes of context on each side, on one
// line, cut on rune boundaries.
func excerpt(text string, loc []int) string {
	start, end := max(0, loc[0]-40), min(len(text), loc[1]+40)
	for start > 0 && !utf8.RuneStart(text[start]) {
		start++
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	s := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}

func (b *Bridge) quarantineDir(dirName string) string {
	return filepath.Join(b.dir, ".git", quarantineDirName, dirName)
}

// Quarantined reports whether a refused chunk of project is waiting.
func (b *Bridge) Quarantined(project string) bool {
	_, err := os.Stat(filepath.Join(b.quarantineDir(ProjectDirName(project)), manifestName))
	return err == nil
}

// quarantine keeps the refused chunks: files first, manifest last, so
// Quarantined only reports a complete one.
func (b *Bridge) quarantine(srcChunks, dirName string, chunks []ChunkEntry) error {
	dir := b.quarantineDir(dirName)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := copyChunkFiles(srcChunks, filepath.Join(dir, chunksDirName), chunks); err != nil {
		return err
	}
	data, err := marshalManifest(Manifest{Version: 1, Chunks: chunks})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, manifestName), data, 0o644)
}

func (b *Bridge) clearQuarantine(dirName string) {
	_ = os.RemoveAll(b.quarantineDir(dirName))
}
