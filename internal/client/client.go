// Package client is the typed HTTP wrapper around the engram local API.
//
// The contract here MUST be the only place that knows about engram routes,
// payload shapes, or auth headers. Handlers, components, and launcher code
// must depend on this package — never on net/http directly against engram.
//
// Routes engram serves that nothing here wraps yet, so the next person does not
// go looking for them elsewhere: POST /observations/passive, POST /prompts,
// DELETE /prompts/{id}, POST /sessions/{id}/end, DELETE /sessions/{id},
// GET /context, GET /review, POST /review/mark_reviewed, GET /doctor,
// GET /project/current, POST /projects/migrate, POST /import,
// GET /conflicts/deferred, POST /conflicts/deferred/replay, POST /conflicts/compare.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		hc:      &http.Client{Timeout: 15 * time.Second},
	}
}

// Health hits GET /health and decodes the version envelope.
// Used at startup for the engram-version compatibility check.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	var out Health
	if err := c.get(ctx, "/health", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Stats(ctx context.Context) (*Stats, error) {
	var out Stats
	if err := c.get(ctx, "/stats", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Search(ctx context.Context, q SearchQuery) (*SearchResponse, error) {
	var out []Observation
	if err := c.get(ctx, "/search", q.values(), &out); err != nil {
		return nil, err
	}
	return &SearchResponse{Results: skip(out, q.Offset)}, nil
}

// RecentObservations lists observations newest first. scope ("project" or
// "personal") is filtered by engram itself — the client used to drop it and
// make callers filter by hand over a wide window.
func (c *Client) RecentObservations(ctx context.Context, project, scope string, limit, offset int) ([]Observation, error) {
	v := url.Values{}
	if project != "" {
		v.Set("project", project)
	}
	if scope != "" {
		v.Set("scope", scope)
	}
	if n := window(limit, offset); n > 0 {
		v.Set("limit", strconv.Itoa(n))
	}
	var out []Observation
	if err := c.get(ctx, "/observations/recent", v, &out); err != nil {
		return nil, err
	}
	return skip(out, offset), nil
}

func (c *Client) GetObservation(ctx context.Context, id int64) (*Observation, error) {
	var out Observation
	if err := c.get(ctx, fmt.Sprintf("/observations/%d", id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Timeline(ctx context.Context, id int64, before, after int) (*TimelineResponse, error) {
	v := url.Values{}
	v.Set("observation_id", fmt.Sprintf("%d", id))
	if before > 0 {
		v.Set("before", fmt.Sprintf("%d", before))
	}
	if after > 0 {
		v.Set("after", fmt.Sprintf("%d", after))
	}
	var out TimelineResponse
	if err := c.get(ctx, "/timeline", v, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	var out Session
	if err := c.get(ctx, "/sessions/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecentSessions lists sessions, newest first. project scopes the list; empty
// means every project. engram has always accepted the filter — this client
// simply never sent it, which is why the Sessions screen used to ignore the
// project switcher every other screen obeys.
func (c *Client) RecentSessions(ctx context.Context, project string, limit, offset int) ([]Session, error) {
	v := url.Values{}
	if project != "" {
		v.Set("project", project)
	}
	if n := window(limit, offset); n > 0 {
		v.Set("limit", strconv.Itoa(n))
	}
	var out []Session
	if err := c.get(ctx, "/sessions/recent", v, &out); err != nil {
		return nil, err
	}
	return skip(out, offset), nil
}

// engram paginates nothing: /search, /observations/recent and /sessions/recent
// all take a limit and have never had an offset. Sending one was a no-op, so
// the Next button showed page 1 again. We emulate it: ask for limit+offset rows
// and drop the head.
//
// ponytail: page N costs N*limit rows over the wire. If that ever hurts, the
// fix is an offset upstream in engram, not a cursor here.
func window(limit, offset int) int {
	if limit <= 0 {
		return 0
	}
	return limit + offset
}

func skip[T any](items []T, offset int) []T {
	switch {
	case offset <= 0:
		return items
	case offset >= len(items):
		return nil
	default:
		return items[offset:]
	}
}

// ── Prompts ────────────────────────────────────────────────────────────────

func (c *Client) RecentPrompts(ctx context.Context, project string, limit int) ([]Prompt, error) {
	v := url.Values{}
	if project != "" {
		v.Set("project", project)
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	var out []Prompt
	if err := c.get(ctx, "/prompts/recent", v, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) SearchPrompts(ctx context.Context, query, project string, limit int) ([]Prompt, error) {
	v := url.Values{}
	v.Set("q", query)
	if project != "" {
		v.Set("project", project)
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	var out []Prompt
	if err := c.get(ctx, "/prompts/search", v, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Export streams the engram /export response body to the caller. The caller
// must close the returned ReadCloser. Caller closes resp.Body.
//
// Uses its own client with no Timeout, because http.Client.Timeout covers
// reading the body too and keeps running after Do returns. On the shared 15 s
// client, any backup that took longer than that to transfer was cut off
// mid-stream — and the handler has already sent 200 with a
// Content-Disposition, so the user gets a truncated .json that looks fine and
// fails the day they try to restore it. Cancellation still works: the request
// carries ctx, which is the browser connection.
func (c *Client) Export(ctx context.Context, project string) (io.ReadCloser, string, error) {
	v := url.Values{}
	if project != "" {
		v.Set("project", project)
	}
	u := c.baseURL + "/export"
	if len(v) > 0 {
		u += "?" + v.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// Same transport, so the connection pool is shared; only the deadline goes.
	streamHC := &http.Client{Transport: c.hc.Transport}
	resp, err := streamHC.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("engram /export: %w", err)
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, "", fmt.Errorf("engram /export: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return resp.Body, resp.Header.Get("Content-Disposition"), nil
}

// ── Save ───────────────────────────────────────────────────────────────────

// CreateSession creates a session if not exists; safe to retry.
func (c *Client) CreateSession(ctx context.Context, id, project, directory string) error {
	body := CreateSessionRequest{ID: id, Project: project, Directory: directory}
	return c.post(ctx, "/sessions", body, nil)
}

// UpdateObservation patches selected fields of an observation.
// Only non-nil pointer fields in req are sent.
func (c *Client) UpdateObservation(ctx context.Context, id int64, req UpdateObservationRequest) (*Observation, error) {
	var out Observation
	if err := c.patch(ctx, fmt.Sprintf("/observations/%d", id), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteObservation removes an observation. hard=true wipes; false soft-deletes.
func (c *Client) DeleteObservation(ctx context.Context, id int64, hard bool) error {
	path := fmt.Sprintf("/observations/%d", id)
	if hard {
		path += "?hard=true"
	}
	return c.delete(ctx, path)
}

// AddObservation posts a new observation. SessionID must already exist.
func (c *Client) AddObservation(ctx context.Context, req AddObservationRequest) (int64, error) {
	var out struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := c.post(ctx, "/observations", req, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// ── Conflicts ───────────────────────────────────────────────────────────────

func (c *Client) Conflicts(ctx context.Context, project, status string, limit, offset int) (*ConflictsPage, error) {
	v := url.Values{}
	if project != "" {
		v.Set("project", project)
	}
	if status != "" {
		v.Set("status", status)
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	var out ConflictsPage
	if err := c.get(ctx, "/conflicts", v, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Conflict(ctx context.Context, id int64) (*ConflictDetail, error) {
	var out ConflictDetail
	if err := c.get(ctx, fmt.Sprintf("/conflicts/%d", id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ConflictStats(ctx context.Context, project string) (*ConflictStats, error) {
	v := url.Values{}
	if project != "" {
		v.Set("project", project)
	}
	var out ConflictStats
	if err := c.get(ctx, "/conflicts/stats", v, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ScanConflicts triggers POST /conflicts/scan. When req.Semantic is true the
// server invokes the configured agent CLI (ENGRAM_AGENT_CLI) to compare each
// candidate pair and persists non-not_conflict verdicts automatically.
//
// Semantic scans can take minutes. Uses a longer-lived HTTP client to avoid
// the 15 s default timeout while the LLM works.
func (c *Client) ScanConflicts(ctx context.Context, req ScanRequest) (*ScanResult, error) {
	longHC := &http.Client{Timeout: 30 * time.Minute}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(req); err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/conflicts/scan", &buf)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := longHC.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("engram /conflicts/scan: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("engram /conflicts/scan: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out ScanResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) JudgeConflict(ctx context.Context, judgmentID, relation, reason string, confidence float64) error {
	body := JudgeRequest{
		JudgmentID: judgmentID,
		Relation:   relation,
		Reason:     reason,
		Confidence: confidence,
	}
	return c.post(ctx, "/conflicts/judge", body, nil)
}

// ── Sync ─────────────────────────────────────────────────────────────────────

// SyncStatus hits GET /sync/status. project may be "" for the global view.
// This is the only sync-related route the LOCAL engram exposes over HTTP; the
// actual push/pull/enroll operations run through the engram CLI (see
// internal/engramcli), since engram has no HTTP trigger for them.
func (c *Client) SyncStatus(ctx context.Context, project string) (*SyncStatus, error) {
	v := url.Values{}
	if project != "" {
		v.Set("project", project)
	}
	var out SyncStatus
	if err := c.get(ctx, "/sync/status", v, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ─── internals ────────────────────────────────────────────────────────────────

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("engram %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("engram %s: %s: %s", path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) patch(ctx context.Context, path string, body any, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("engram %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("engram %s: %s: %s", path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) delete(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("engram %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("engram %s: %s: %s", path, resp.Status, strings.TrimSpace(string(raw)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("engram %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("engram %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		// Drain before closing, like post/patch/delete already do: a body left
		// unread makes net/http drop the connection instead of returning it to
		// the keep-alive pool.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
