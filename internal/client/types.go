package client

import (
	"net/url"
	"strconv"
)

// These structs mirror the engram HTTP API JSON envelopes. They are intentionally
// permissive — unknown fields are tolerated by encoding/json so engram can add
// fields upstream without breaking us.

type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

type Stats struct {
	Observations int64    `json:"total_observations"`
	Sessions     int64    `json:"total_sessions"`
	Prompts      int64    `json:"total_prompts"`
	Projects     []string `json:"projects"`
}

type Observation struct {
	ID            int64   `json:"id"`
	SyncID        string  `json:"sync_id,omitempty"`
	SessionID     string  `json:"session_id,omitempty"`
	Type          string  `json:"type"`
	Title         string  `json:"title"`
	Content       string  `json:"content"`
	Project       string  `json:"project"`
	Scope         string  `json:"scope,omitempty"`
	TopicKey      string  `json:"topic_key,omitempty"`
	RevisionCount int     `json:"revision_count,omitempty"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at,omitempty"`
	DeletedAt     *string `json:"deleted_at,omitempty"`
}

type Session struct {
	ID               string  `json:"id"`
	Project          string  `json:"project"`
	Directory        string  `json:"directory,omitempty"`
	StartedAt        string  `json:"started_at"`
	EndedAt          *string `json:"ended_at,omitempty"`
	Summary          string  `json:"summary,omitempty"`
	ObservationCount int64   `json:"observation_count,omitempty"`
}

type SearchQuery struct {
	Query   string
	Project string
	Scope   string
	Type    string
	Limit   int
	Offset  int
}

func (q SearchQuery) values() url.Values {
	v := url.Values{}
	if q.Query != "" {
		v.Set("q", q.Query)
	}
	if q.Project != "" {
		v.Set("project", q.Project)
	}
	if q.Scope != "" {
		v.Set("scope", q.Scope)
	}
	if q.Type != "" {
		v.Set("type", q.Type)
	}
	if n := window(q.Limit, q.Offset); n > 0 {
		v.Set("limit", strconv.Itoa(n))
	}
	return v
}

type SearchResponse struct {
	Results []Observation `json:"results"`
}

type TimelineResponse struct {
	Focus  Observation   `json:"focus"`
	Before []Observation `json:"before"`
	After  []Observation `json:"after"`
}

type Prompt struct {
	ID        int64  `json:"id"`
	SyncID    string `json:"sync_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Content   string `json:"content"`
	Project   string `json:"project,omitempty"`
	CreatedAt string `json:"created_at"`
}

type AddObservationRequest struct {
	SessionID string `json:"session_id"`
	Type      string `json:"type,omitempty"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Project   string `json:"project,omitempty"`
	Scope     string `json:"scope,omitempty"`
	TopicKey  string `json:"topic_key,omitempty"`
}

type UpdateObservationRequest struct {
	Type     *string `json:"type,omitempty"`
	Title    *string `json:"title,omitempty"`
	Content  *string `json:"content,omitempty"`
	Project  *string `json:"project,omitempty"`
	Scope    *string `json:"scope,omitempty"`
	TopicKey *string `json:"topic_key,omitempty"`
}

type CreateSessionRequest struct {
	ID        string `json:"id"`
	Project   string `json:"project"`
	Directory string `json:"directory,omitempty"`
}

// ── Conflicts ───────────────────────────────────────────────────────────────

type ConflictListItem struct {
	ID             int64  `json:"id"`
	SyncID         string `json:"sync_id"`
	Relation       string `json:"relation"`
	JudgmentStatus string `json:"judgment_status"`
	SourceID       string `json:"source_id"`
	SourceTitle    string `json:"source_title"`
	TargetID       string `json:"target_id"`
	TargetTitle    string `json:"target_title"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

type ConflictsPage struct {
	Total     int                `json:"total"`
	Limit     int                `json:"limit"`
	Offset    int                `json:"offset"`
	Relations []ConflictListItem `json:"relations"`
}

type ConflictDetail struct {
	RelationID     int64  `json:"relation_id"`
	SyncID         string `json:"sync_id"`
	Relation       string `json:"relation"`
	JudgmentStatus string `json:"judgment_status"`
	SourceID       string `json:"source_id"`
	SourceSnippet  string `json:"source_snippet"`
	TargetID       string `json:"target_id"`
	TargetSnippet  string `json:"target_snippet"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at,omitempty"`
	// engram's GET /conflicts/{id} does not return the stored reason or
	// confidence — only the columns above. Do not add fields it never sends.
}

type ConflictStats struct {
	Project          string         `json:"project"`
	ByRelation       map[string]int `json:"by_relation"`
	ByJudgmentStatus map[string]int `json:"by_judgment_status"`
	Deferred         int            `json:"deferred"`
	Dead             int            `json:"dead"`
}

type ScanRequest struct {
	Project               string `json:"project"`
	Apply                 bool   `json:"apply"`
	Semantic              bool   `json:"semantic,omitempty"`
	MaxInsert             int    `json:"max_insert,omitempty"`
	MaxSemantic           int    `json:"max_semantic,omitempty"`
	Concurrency           int    `json:"concurrency,omitempty"`
	TimeoutPerCallSeconds int    `json:"timeout_per_call_seconds,omitempty"`
	Since                 string `json:"since,omitempty"`
}

type ScanResult struct {
	Project         string `json:"project"`
	Inspected       int    `json:"inspected"`
	CandidatesFound int    `json:"candidates_found"`
	AlreadyRelated  int    `json:"already_related"`
	Inserted        int    `json:"inserted"`
	Capped          bool   `json:"capped"`
	DryRun          bool   `json:"dry_run"`
	SemanticJudged  int    `json:"semantic_judged"`
	SemanticSkipped int    `json:"semantic_skipped"`
	SemanticErrors  int    `json:"semantic_errors"`
	Warning         string `json:"warning,omitempty"`
}

type JudgeRequest struct {
	JudgmentID string  `json:"judgment_id"`
	Relation   string  `json:"relation"`
	Reason     string  `json:"reason,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// ── Sync status ──────────────────────────────────────────────────────────────

// SyncStatus mirrors the local engram GET /sync/status envelope. When background
// sync is not configured, engram returns {"enabled":false,"message":...} — the
// extra Message field is captured so the UI can show why it's off.
type SyncStatus struct {
	Enabled             bool         `json:"enabled"`
	Message             string       `json:"message,omitempty"`
	Phase               string       `json:"phase,omitempty"`
	LastError           string       `json:"last_error,omitempty"`
	ConsecutiveFailures int          `json:"consecutive_failures,omitempty"`
	BackoffUntil        *string      `json:"backoff_until,omitempty"`
	LastSyncAt          *string      `json:"last_sync_at,omitempty"`
	ReasonCode          string       `json:"reason_code,omitempty"`
	ReasonMessage       string       `json:"reason_message,omitempty"`
	DeferredCount       int          `json:"deferred_count,omitempty"`
	DeadCount           int          `json:"dead_count,omitempty"`
	Upgrade             *SyncUpgrade `json:"upgrade,omitempty"`
}

type SyncUpgrade struct {
	Stage         string `json:"stage,omitempty"`
	ReasonCode    string `json:"reason_code,omitempty"`
	ReasonMessage string `json:"reason_message,omitempty"`
}
