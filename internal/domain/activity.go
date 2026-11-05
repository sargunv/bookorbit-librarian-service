package domain

import "encoding/json"

type ActivityEvent struct {
	ID        int64           `json:"id"`
	JobID     int64           `json:"jobId"`
	BookID    int             `json:"bookId"`
	Policy    string          `json:"policy"`
	Kind      string          `json:"kind"`
	Stage     string          `json:"stage,omitempty"`
	Title     string          `json:"title,omitempty"`
	Message   string          `json:"message"`
	Details   ActivityDetails `json:"details"`
	CreatedAt string          `json:"createdAt"`
}

type ActivityDetails struct {
	Coverage     *Coverage     `json:"coverage,omitempty"`
	Changes      []FieldChange `json:"changes,omitempty"`
	Reason       string        `json:"reason,omitempty"`
	GroupReason  string        `json:"groupReason,omitempty"`
	CoverChanged bool          `json:"coverChanged,omitempty"`
	CoverSource  string        `json:"coverSource,omitempty"`
	Sources      []string      `json:"sources,omitempty"`
	MovedFiles   int           `json:"movedFiles,omitempty"`
	MergedBooks  []int         `json:"mergedBooks,omitempty"`
	Warnings     []string      `json:"warnings,omitempty"`
	Error        string        `json:"error,omitempty"`
}

type FieldChange struct {
	Field  string          `json:"field"`
	Before json.RawMessage `json:"before"`
	After  json.RawMessage `json:"after"`
	Source string          `json:"source,omitempty"`
}

type ActivitySummary struct {
	ThroughAt    string             `json:"throughAt"`
	ID           int64              `json:"id"`
	FromEvent    int64              `json:"fromEvent"`
	ToEvent      int64              `json:"toEvent"`
	Policy       string             `json:"policy"`
	Highlights   []SummaryHighlight `json:"highlights"`
	Model        string             `json:"model"`
	InputTokens  int                `json:"inputTokens"`
	OutputTokens int                `json:"outputTokens"`
	CreatedAt    string             `json:"createdAt"`
}

type SummaryHighlight struct {
	Text     string  `json:"text"`
	EventIDs []int64 `json:"eventIds"`
}
