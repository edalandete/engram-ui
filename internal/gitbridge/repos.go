package gitbridge

// Several shared repositories at once, and which one each project publishes to.
//
// One repository per team or per client is the normal shape, and a project
// belongs to exactly one of them. Each repository is still an ordinary Bridge
// with its own clone and its own lock; this file only finds them and remembers
// the mapping.
//
// Every clone is a sibling in the engram data directory:
//
//	<data>/share-repo                the single clone of older versions, kept as is
//	<data>/share-repo-<name>         one per repository added since
//	<data>/engram-ui-share.json      project → repository, explicit choices only
//	<data>/engram-ui-imported.json   the ledger, shared by every clone
//
// Siblings rather than a subdirectory, so the ledger (which lives next to its
// clone) is one file for all of them. Sharing it is correct, not merely
// convenient: it records what this DATABASE has applied and published, and a
// chunk id is a content hash, so an id from one repository can only match the
// same content in another.
//
// The clones are still the configuration — a repository is connected when its
// directory holds a .git. Only the project mapping needs a file, because it is
// a choice nothing on disk could reveal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/edalandete/engram-ui/internal/engramcli"
	"github.com/edalandete/engram-ui/internal/gitcli"
)

const (
	repoDirPrefix  = "share-repo"
	assignFileName = "engram-ui-share.json"
)

// Repos is every shared repository this machine is connected to.
type Repos struct {
	cli  *engramcli.Runner
	git  *gitcli.Runner
	base string

	// mu guards bridges and the mapping file. Never held across a git call.
	mu      sync.Mutex
	bridges map[string]*Bridge
}

// NewRepos manages the clones in the engram data directory.
func NewRepos(cli *engramcli.Runner, git *gitcli.Runner) *Repos {
	return &Repos{cli: cli, git: git, base: filepath.Dir(RepoDir()), bridges: map[string]*Bridge{}}
}

// Dir is the directory the clones live in.
func (r *Repos) Dir() string { return r.base }

// ID names a repository: its clone's directory name.
func (b *Bridge) ID() string { return filepath.Base(b.dir) }

// bridge returns the one Bridge for id. One per clone for the life of the
// process, because the publish lock and the chunk cache only work if every
// request for that clone shares them.
func (r *Repos) bridge(id string) *Bridge {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.bridges[id]
	if b == nil {
		b = &Bridge{cli: r.cli, git: r.git, dir: filepath.Join(r.base, id), syncIDs: map[string][]chunkMemory{}}
		r.bridges[id] = b
	}
	return b
}

// List returns the connected repositories, ordered by directory name. No git
// call: a clone has a .git and a failed clone leaves none behind.
func (r *Repos) List() []*Bridge {
	entries, err := os.ReadDir(r.base)
	if err != nil {
		return nil
	}
	var out []*Bridge
	for _, e := range entries {
		if e.IsDir() && validID(e.Name()) && isClone(filepath.Join(r.base, e.Name())) {
			out = append(out, r.bridge(e.Name()))
		}
	}
	return out
}

// Get returns the connected repository called id, or nil. id arrives from a
// form, so it is checked to be one of our directory names before it gets near
// a path.
func (r *Repos) Get(id string) *Bridge {
	if !validID(id) || !isClone(filepath.Join(r.base, id)) {
		return nil
	}
	return r.bridge(id)
}

func validID(id string) bool {
	return (id == repoDirPrefix || strings.HasPrefix(id, repoDirPrefix+"-")) && ProjectDirName(id) == id
}

func isClone(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// Add clones url as one more repository. The same remote twice is refused: it
// would only split one repository's projects between two clones of it.
func (r *Repos) Add(ctx context.Context, url string) gitcli.Result {
	url = strings.TrimSpace(url)
	if url == "" {
		return gitcli.Result{Err: errors.New("repository URL is required")}
	}
	for _, b := range r.List() {
		if b.RemoteURL(ctx) == url {
			return gitcli.Result{Err: fmt.Errorf("%s is already connected", url)}
		}
	}
	return r.bridge(r.freeID(url)).Setup(ctx, url)
}

// freeID picks the directory for a new clone: named after the repository so
// the data dir stays readable, numbered when two remotes share a name.
func (r *Repos) freeID(url string) string {
	name := strings.TrimSuffix(strings.TrimRight(url, `/\`), ".git")
	if i := strings.LastIndexAny(name, `/\:`); i >= 0 {
		name = name[i+1:]
	}
	id := repoDirPrefix + "-" + ProjectDirName(name)
	try := id
	for n := 2; !emptyOrMissing(filepath.Join(r.base, try)); n++ {
		try = fmt.Sprintf("%s-%d", id, n)
	}
	return try
}

// emptyOrMissing: Setup clears an empty leftover itself, so it counts as free.
func emptyOrMissing(dir string) bool {
	entries, err := os.ReadDir(dir)
	return os.IsNotExist(err) || (err == nil && len(entries) == 0)
}

// Remove disconnects one repository and forgets the projects assigned to it. A
// later repository can land in the same directory, and a leftover mapping
// would then publish those projects somewhere nobody chose.
func (r *Repos) Remove(id string) error {
	b := r.Get(id)
	if b == nil {
		return fmt.Errorf("no connected repository %q", id)
	}
	if err := b.Disconnect(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.readAssignments()
	for project, to := range m {
		if to == id {
			delete(m, project)
		}
	}
	return r.writeAssignments(m)
}

// Assign makes project publish to the repository id from now on.
func (r *Repos) Assign(project, id string) error {
	if strings.TrimSpace(project) == "" {
		return errors.New("project is required")
	}
	if r.Get(id) == nil {
		return fmt.Errorf("no connected repository %q", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.readAssignments()
	m[ProjectDirName(project)] = id
	return r.writeAssignments(m)
}

// For returns the repository project publishes to, or nil when that is not
// settled. An explicit choice wins; otherwise the only repository there is;
// otherwise the only one that already holds the project. Anything else is a
// question for the user — guessing would publish a project to a repository
// nobody picked, and everyone with access to it would read it.
func (r *Repos) For(project string) *Bridge {
	if strings.TrimSpace(project) == "" {
		return nil
	}
	dirName := ProjectDirName(project)
	r.mu.Lock()
	chosen := r.readAssignments()[dirName]
	r.mu.Unlock()

	repos := r.List()
	for _, b := range repos {
		if b.ID() == chosen {
			return b
		}
	}
	if len(repos) == 1 {
		return repos[0]
	}
	var holder *Bridge
	for _, b := range repos {
		if _, err := os.Stat(b.manifestPath(dirName)); err == nil {
			if holder != nil {
				return nil
			}
			holder = b
		}
	}
	return holder
}

type assignments struct {
	Projects map[string]string `json:"projects"`
}

// readAssignments loads the mapping. A missing or corrupt file reads as empty,
// which only means For falls back to what is on disk or asks again. Callers
// hold r.mu.
func (r *Repos) readAssignments() map[string]string {
	var a assignments
	if data, err := os.ReadFile(filepath.Join(r.base, assignFileName)); err == nil {
		_ = json.Unmarshal(data, &a)
	}
	if a.Projects == nil {
		a.Projects = map[string]string{}
	}
	return a.Projects
}

func (r *Repos) writeAssignments(m map[string]string) error {
	data, err := json.MarshalIndent(assignments{Projects: m}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.base, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.base, assignFileName), data, 0o644)
}
