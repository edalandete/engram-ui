// Package launcher manages the engram subprocess lifecycle.
//
// Two modes:
//   - Embedded: the engram binary is built from the third_party/engram submodule
//     by build-engram.ps1 and shipped via go:embed. Spawn extracts it to the
//     OS user cache dir and starts `engram serve <port>` as a child process,
//     killed on shutdown.
//   - External: engram-ui assumes engram serve is already running. The launcher
//     only health-checks the URL.
package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/edalandete/engram-ui/internal/client"
)

type Launcher struct {
	cmd     *exec.Cmd
	url     string
	binPath string

	// One reaper, many watchers. cmd.Wait may only be called once, and both
	// WaitReady (to notice a child that died at startup) and Stop (to know
	// when it is really gone) need to know. So a single goroutine waits,
	// records the result, and closes done — which any number of receivers can
	// read, any number of times. Writing waitErr before the close is what
	// makes reading it after safe.
	done    chan struct{}
	waitErr error
}

func New() *Launcher { return &Launcher{} }

// URL returns the URL of the spawned engram, or "" if Spawn was not called.
func (l *Launcher) URL() string { return l.url }

// BinPath returns the path to the engram binary backing the running server,
// or "" in external mode (engram was not spawned by us). Callers that need to
// shell out to the engram CLI (see internal/engramcli) should fall back to
// "engram" on PATH when this is empty.
func (l *Launcher) BinPath() string { return l.binPath }

// Spawn extracts the embedded engram binary, picks a free local port, and
// starts `engram serve <port>` as a child process. Returns the base URL the
// subprocess listens on. Stop must be called to terminate the child.
//
// token, when set, is handed to the child as ENGRAM_HTTP_TOKEN. It has to be
// passed explicitly because the child only inherits what is in our
// environment: a token given on the command line as -engram-token never
// reached it, so engram served its API with no authentication at all while
// this UI dutifully sent an Authorization header it was ignoring. A false
// sense of protection is worse than none.
func (l *Launcher) Spawn(ctx context.Context, token string) (string, error) {
	// Idempotent: a second call would overwrite l.cmd and leak the first
	// child, which nothing could then stop. Only ever called once today; this
	// is here so that stays true by construction.
	if l.cmd != nil {
		return l.url, nil
	}
	binPath, err := extractEmbeddedBinary()
	if err != nil {
		return "", err
	}
	port, err := freePort()
	if err != nil {
		return "", fmt.Errorf("pick free port: %w", err)
	}

	cmd := exec.CommandContext(ctx, binPath, "serve", fmt.Sprintf("%d", port))
	// Prefixed, because both processes write to the same stderr and engram-ui's
	// own lines carry "engram-ui ". Without this the two are indistinguishable
	// in exactly the situation you read the log for.
	child := &prefixWriter{w: os.Stderr, prefix: []byte("engram | ")}
	cmd.Stdout = child
	cmd.Stderr = child
	// Via the environment, never argv: a token on a command line is visible to
	// every process on the machine. Same reasoning as engramcli does for the
	// cloud token.
	if token != "" {
		cmd.Env = append(os.Environ(), "ENGRAM_HTTP_TOKEN="+token)
	}
	// CommandContext only covers an orderly cancellation. These two tie the
	// child to this process at the OS level, so it dies with us even when we
	// are killed outright — see reap_windows.go. Two hooks because the
	// platforms need opposite timing: Pdeathsig has to be set before Start, a
	// Job Object can only take a process that already exists.
	prepareReap(cmd)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start engram: %w", err)
	}
	adoptReap(cmd)

	l.done = make(chan struct{})
	go func() {
		l.waitErr = cmd.Wait()
		close(l.done)
	}()

	l.cmd = cmd
	l.binPath = binPath
	l.url = fmt.Sprintf("http://127.0.0.1:%d", port)
	return l.url, nil
}

// Stop terminates the spawned subprocess, if any.
//
// Asks before insisting. engram installs a signal handler and closes its
// SQLite on the way out (cmd/engram/main.go); going straight to Kill —
// TerminateProcess on Windows, SIGKILL on Unix, neither interceptable — meant
// that handler never ran and the WAL was never checkpointed. Recoverable, WAL
// is built for it, but it made every single exit an unclean one and let the
// -wal file grow across sessions for no reason.
//
// Two seconds is the whole budget: this runs on the way out, and a shutdown
// that hangs on a courtesy is worse than an unclean one.
func (l *Launcher) Stop() error {
	if l.cmd == nil || l.cmd.Process == nil {
		return nil
	}
	if err := l.cmd.Process.Signal(os.Interrupt); err == nil {
		select {
		case <-l.done:
			return nil
		case <-time.After(2 * time.Second):
		}
	}
	// Either the signal is unsupported (Windows does not deliver os.Interrupt
	// to another process) or engram did not act on it in time.
	_ = l.cmd.Process.Kill()
	<-l.done
	return nil
}

// WaitReady polls /health until it returns 200 or the context is cancelled.
//
// Also watches the subprocess. If engram exits immediately — SQLite locked by
// another instance, the port taken between picking it and binding it, a
// truncated binary — polling alone burns the full 15 s and then reports
// "engram not reachable at http://127.0.0.1:54321", naming an ephemeral port
// the user never chose and suggesting they start something they did not start.
// The exit status says what actually happened, so say that instead.
func (l *Launcher) WaitReady(ctx context.Context, c *client.Client, timeout time.Duration) (*client.Health, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		h, err := c.Health(ctx)
		if err == nil {
			return h, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.done:
			return nil, fmt.Errorf("the engram subprocess exited during startup: %v", l.waitErr)
		case <-time.After(200 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("timeout")
	}
	return nil, fmt.Errorf("engram not reachable: %w", lastErr)
}

// extractEmbeddedBinary writes the embedded engram binary to the OS user cache
// dir (idempotent: skipped when the cached copy is already identical) and
// returns its path.
func extractEmbeddedBinary() (string, error) {
	name := "engram"
	if runtime.GOOS == "windows" {
		name = "engram.exe"
	}
	embedPath := "embedded/" + name
	data, err := fs.ReadFile(embeddedFS, embedPath)
	if err != nil {
		return "", fmt.Errorf("embedded engram binary missing — build with build-engram.ps1: %w", err)
	}

	// No fallback to os.TempDir(). UserCacheDir fails when HOME or
	// %LocalAppData% is unset — normal under systemd, in containers and under
	// cron — and /tmp is world-writable and shared. Landing there means
	// another user can pre-create the directory, or win the gap between
	// needWrite deciding "unchanged" and exec running the file, and we execute
	// their binary with our privileges. Failing here is visible; degrading
	// quietly into a shared directory is not.
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("no user cache directory (is HOME set?): %w", err)
	}
	// 0700, not 0755: a private copy of a binary only this user runs.
	outDir := filepath.Join(cacheDir, "engram-ui")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir cache: %w", err)
	}
	out := filepath.Join(outDir, name)

	if needWrite(out, data) {
		if err := writeAtomic(out, data); err != nil {
			return "", fmt.Errorf("write %s: %w", out, err)
		}
	}
	return out, nil
}

// writeAtomic writes via a temp file and a rename, so the destination is never
// a half-written binary.
//
// os.WriteFile truncates in place. Two instances starting right after a
// rebuild would both decide the cached copy differs and interleave 20 MB of
// writes onto the same inode — and then one of them execs it. The PID in the
// temp name keeps the two from colliding on the scratch file too.
func writeAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o700); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		// Windows refuses to replace a file that is running, and so does Linux
		// with ETXTBSY. Both mean a previous engram-ui is still using this
		// copy, which is worth saying plainly.
		return fmt.Errorf("%w (another engram-ui may be running)", err)
	}
	return nil
}

// needWrite reports whether the cached binary differs from the embedded one.
//
// Compares content, not size. Two engram builds a few commits apart are
// routinely byte-for-byte the same length, and a size-only check then keeps
// running the previous binary forever after a submodule bump — the failure
// looks like "my MinEngram bump did nothing". The old comment promised
// "size + modtime"; modtime never entered into it, and would have been
// useless anyway since extraction stamps it with the extraction time.
//
// Costs one read of ~20 MB per launch. Paid once at startup, against a bug
// that is invisible when it happens.
func needWrite(path string, data []byte) bool {
	existing, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	return !bytes.Equal(existing, data)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// prefixWriter tags each line the child writes, so engram's output and
// engram-ui's own log lines can be told apart on a shared stderr.
//
// Buffers a partial line rather than prefixing every Write: a child that
// flushes mid-line would otherwise get a prefix inserted into the middle of
// its own sentence.
type prefixWriter struct {
	w      io.Writer
	prefix []byte
	mu     sync.Mutex
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := append(append([]byte{}, p.prefix...), p.buf[:i+1]...)
		if _, err := p.w.Write(line); err != nil {
			return len(b), err
		}
		p.buf = p.buf[i+1:]
	}
}
