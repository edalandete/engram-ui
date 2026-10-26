// Package cloudclient is a read-only HTTP client for the engram CLOUD server.
//
// This is the THIRD boundary to engram. The other two:
//   - internal/client — typed HTTP to the LOCAL engram server (read/write data).
//   - internal/engramcli — execs the engram binary for cloud-sync CLI ops.
//
// This package talks directly to the CLOUD server's read endpoints
// (GET /sync/pull, GET /sync/pull/{chunkID}) to fetch the chunk manifest and
// individual chunks. It is used ONLY by internal/cloudbridge to implement
// selective pull (download a subset of a project's chunks, filtered by author,
// then apply them via a local import). It never writes to the cloud.
//
// We cannot import engram packages (repo rule), so the Manifest/ChunkEntry
// structs here mirror engram's internal/sync wire shape for JSON decoding.
package cloudclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ChunkEntry mirrors engram internal/sync.ChunkEntry (the manifest entry shape).
// CreatedBy is the username/host that pushed the chunk — the field we filter on.
type ChunkEntry struct {
	ID        string `json:"id"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	Sessions  int    `json:"sessions"`
	Memories  int    `json:"memories"`
	Prompts   int    `json:"prompts"`
}

// Manifest mirrors engram internal/sync.Manifest.
type Manifest struct {
	Version int          `json:"version"`
	Chunks  []ChunkEntry `json:"chunks"`
}

// Ceilings on what a remote is allowed to make this process allocate. Everything
// past this point is the cloud server's data, and a client that reads without a
// limit lets whoever runs that server decide how much memory we spend. Generous
// on purpose — the largest real chunk seen is a few hundred KB — so these are a
// backstop against a hostile or broken server, not a working constraint.
const (
	maxManifestBytes = 8 << 20   // 8 MiB
	maxChunkBytes    = 128 << 20 // 128 MiB
)

// Typed errors so callers can branch on auth/availability.
var (
	ErrUnauthorized   = errors.New("cloudclient: unauthorized (check cloud token)")
	ErrForbidden      = errors.New("cloudclient: forbidden (project not allowed for this token)")
	ErrChunkNotFound  = errors.New("cloudclient: chunk not found")
	ErrServerRequired = errors.New("cloudclient: cloud server URL is required")
)

// Client is a read-only HTTP client bound to one cloud server + token.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New validates the server URL (http/https, host required, no query/fragment)
// and returns a client. token may be empty (insecure local-dev cloud).
func New(serverURL, token string) (*Client, error) {
	base, err := validateBaseURL(serverURL)
	if err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	// http is allowed so a local insecure cloud needs no certificate — that is
	// what the scheme check above is lenient for. Sending a real bearer token
	// over it is a different thing: the token goes out in clear on every
	// manifest and every chunk, and the same channel hands back the manifest
	// we then trust for filenames. Loopback is exempt; it does not leave the
	// machine.
	if token != "" {
		if err := requireSecureTransport(base); err != nil {
			return nil, err
		}
	}
	return &Client{
		base:  base,
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// requireSecureTransport rejects a token over plaintext to a non-local host.
func requireSecureTransport(base string) error {
	parsed, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("cloudclient: invalid server url: %w", err)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := parsed.Hostname()
	if host == "localhost" || net.ParseIP(host).IsLoopback() {
		return nil
	}
	return fmt.Errorf("cloudclient: refusing to send the cloud token in clear to %s — use https, or drop the token for a local insecure cloud", parsed.Host)
}

func validateBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrServerRequired
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("cloudclient: invalid server url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("cloudclient: invalid server url: scheme must be http or https")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("cloudclient: invalid server url: host is required")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (c *Client) endpointURL(query url.Values, parts ...string) (string, error) {
	endpoint, err := url.JoinPath(c.base, parts...)
	if err != nil {
		return "", fmt.Errorf("cloudclient: build request url: %w", err)
	}
	if len(query) == 0 {
		return endpoint, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("cloudclient: build request url: %w", err)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func (c *Client) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// statusError maps a non-2xx response to a typed error. notFound is the sentinel
// returned for 404 (differs per endpoint: chunk-not-found vs project-not-found).
func statusError(op string, code int, body []byte, notFound error) error {
	switch code {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return notFound
	}
	// Truncate by runes, not bytes: this text comes from a remote server and
	// lands in a banner in the UI, so cutting mid-rune puts invalid UTF-8 on
	// screen as a replacement character.
	msg := strings.TrimSpace(string(body))
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300]) + "…"
	}
	return fmt.Errorf("cloudclient: %s: status %d: %s", op, code, msg)
}

// ReadManifest fetches the chunk manifest for a project. The cloud server
// normalizes the project name, so we send it as-is.
func (c *Client) ReadManifest(ctx context.Context, project string) (*Manifest, error) {
	reqURL, err := c.endpointURL(url.Values{"project": []string{project}}, "sync", "pull")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cloudclient: build manifest request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudclient: fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, statusError("fetch manifest", resp.StatusCode, body, fmt.Errorf("cloudclient: project %q not found on cloud", project))
	}

	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifestBytes)).Decode(&m); err != nil {
		return nil, fmt.Errorf("cloudclient: parse manifest: %w", err)
	}
	// Every id is about to become a path segment on this machine and a path
	// component of a URL. Reject the whole manifest rather than the bad entry:
	// a manifest carrying one hostile id is not a manifest to trust the rest of.
	for _, e := range m.Chunks {
		if !validChunkID(e.ID) {
			return nil, fmt.Errorf("cloudclient: manifest contains an invalid chunk id %q", e.ID)
		}
	}
	return &m, nil
}

// validChunkID accepts exactly what engram produces and nothing else.
//
// engram derives the id as hex.EncodeToString(sha256(payload))[:8]
// (chunkcodec.ChunkID), so the shape is closed: eight lowercase hex digits.
// That matters because callers paste the id straight into a filesystem path —
// filepath.Join CLEANS a path but does not CONFINE it, so an id of
// "../../../../evil" resolves clean out of the temp directory and truncates
// whatever is there. Checking the format is cheaper and tighter than trying to
// prove a path stayed inside a root.
func validChunkID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ReadChunk fetches one chunk's raw JSON bytes (NOT gzipped — the cloud serves
// plain JSON; the caller gzips before writing to the local .engram/ layout).
func (c *Client) ReadChunk(ctx context.Context, project, chunkID string) ([]byte, error) {
	// Checked again here, not only in Manifest: this is exported, and the id
	// ends up both in a URL path and in a filename on disk.
	if !validChunkID(chunkID) {
		return nil, fmt.Errorf("cloudclient: invalid chunk id %q", chunkID)
	}
	reqURL, err := c.endpointURL(url.Values{"project": []string{project}}, "sync", "pull", chunkID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cloudclient: build chunk request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudclient: fetch chunk %s: %w", chunkID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, statusError(fmt.Sprintf("fetch chunk %s", chunkID), resp.StatusCode, body, ErrChunkNotFound)
	}

	// Capped: a chunk is written straight to disk and then handed to engram's
	// importer, so an unbounded body is the remote's choice of how much of
	// this machine's memory and disk to spend. Read one byte past the cap to
	// tell "exactly at the limit" from "truncated here".
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxChunkBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cloudclient: read chunk %s body: %w", chunkID, err)
	}
	if int64(len(data)) > maxChunkBytes {
		return nil, fmt.Errorf("cloudclient: chunk %s exceeds %d bytes", chunkID, maxChunkBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("cloudclient: chunk %s returned empty body", chunkID)
	}
	return data, nil
}
