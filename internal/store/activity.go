package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

const (
	activityColumns = `id,job_id,book_id,policy,kind,stage,title,message,details,created_at`
	visibleKinds    = `('applied','skipped','retry','failed','planned')`
)

type executor interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

func recordEvent(ctx context.Context, db executor, e domain.ActivityEvent) error {
	raw, err := json.Marshal(e.Details)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO activity(job_id,book_id,policy,kind,stage,title,message,details,created_at) VALUES(?,?,?,?,?,?,?,?,coalesce(nullif(?,''),strftime('%Y-%m-%dT%H:%M:%fZ','now')))`, e.JobID, e.BookID, e.Policy, e.Kind, e.Stage, e.Title, e.Message, string(raw), e.CreatedAt)
	return err
}

func (s *Store) RecordEvent(ctx context.Context, e domain.ActivityEvent) error {
	return recordEvent(ctx, s.db, e)
}

func eventForJob(j Job, kind, message string) domain.ActivityEvent {
	e := domain.ActivityEvent{JobID: j.ID, BookID: j.BookID, Policy: j.Policy, Kind: kind, Message: message}
	if kind == "retry" || kind == "failed" {
		e.Details.Error = message
	}
	if p := j.Proposal; p != nil {
		e.Title = p.Book.Metadata.Title
		e.Details.Reason = p.Decision.Reason
		e.Details.GroupReason = p.SeriesReason
		e.Details.Warnings = p.Warnings
		e.Details.CoverSource = p.Provenance["cover"]
		sources := map[string]bool{}
		indices := append([]int{p.Decision.Match}, p.Decision.WorkSources...)
		indices = append(indices, p.Decision.EditionSources...)
		for _, index := range indices {
			if index > 0 && index <= len(p.Candidates) {
				candidate := p.Candidates[index-1]
				sources[candidate.Provider+"/"+candidate.ProviderID] = true
			}
		}
		for _, source := range p.Provenance {
			sources[source] = true
		}
		for source := range sources {
			e.Details.Sources = append(e.Details.Sources, source)
		}
		sort.Strings(e.Details.Sources)
		if kind == "applied" || kind == "planned" {
			raw, _ := json.Marshal(p.Book.Metadata)
			before := map[string]json.RawMessage{}
			_ = json.Unmarshal(raw, &before)
			for _, key := range []string{"hardcover", "goodreads", "google", "openLibrary"} {
				if value := p.Book.ProviderIDs[key]; value != "" {
					field := map[string]string{"hardcover": "hardcoverId", "goodreads": "goodreadsId", "google": "googleBooksId", "openLibrary": "openLibraryId"}[key]
					before[field], _ = json.Marshal(value)
				}
			}
			if p.Book.HardcoverEditionID != "" {
				before["hardcoverEditionId"], _ = json.Marshal(p.Book.HardcoverEditionID)
			}
			fields := make([]string, 0, len(p.Patch))
			for field := range p.Patch {
				fields = append(fields, field)
			}
			sort.Strings(fields)
			for _, field := range fields {
				old := before[field]
				if len(old) == 0 {
					old = json.RawMessage("null")
				}
				e.Details.Changes = append(e.Details.Changes, domain.FieldChange{Field: field, Before: old, After: p.Patch[field], Source: p.Provenance[field]})
			}
			e.Details.CoverChanged = p.CoverURL != ""
		}
	}
	var result struct {
		Moved  int   `json:"movedFileCount"`
		Merged []int `json:"mergedBookIds"`
	}
	if kind == "applied" && len(j.Result) > 0 && json.Unmarshal(j.Result, &result) == nil {
		e.Details.MovedFiles = result.Moved
		e.Details.MergedBooks = result.Merged
	}
	if e.Message == "" {
		e.Message = e.Details.Reason
	}
	return e
}

func readEvent(row scanner) (domain.ActivityEvent, error) {
	var e domain.ActivityEvent
	var raw string
	err := row.Scan(&e.ID, &e.JobID, &e.BookID, &e.Policy, &e.Kind, &e.Stage, &e.Title, &e.Message, &raw, &e.CreatedAt)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &e.Details)
	}
	return e, err
}

func (s *Store) Activity(ctx context.Context, policy string, before int64, limit int) ([]domain.ActivityEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+activityColumns+` FROM activity WHERE (?='' OR policy=?) AND kind IN `+visibleKinds+` AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, policy, policy, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	events := []domain.ActivityEvent{}
	for rows.Next() {
		e, err := readEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) ActivityAfter(ctx context.Context, policy string, after int64, limit int) ([]domain.ActivityEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+activityColumns+` FROM activity WHERE policy=? AND kind IN `+visibleKinds+` AND id>? ORDER BY id LIMIT ?`, policy, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	events := []domain.ActivityEvent{}
	for rows.Next() {
		e, err := readEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) ActiveActivity(ctx context.Context, policy string) ([]domain.ActivityEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+activityColumns+` FROM activity a WHERE policy=? AND kind='progress' AND id=(SELECT max(id) FROM activity WHERE job_id=a.job_id AND kind='progress') AND EXISTS(SELECT 1 FROM jobs WHERE id=a.job_id AND status='running') ORDER BY id DESC`, policy)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	events := []domain.ActivityEvent{}
	for rows.Next() {
		e, err := readEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) PolicyCounts(ctx context.Context, policy string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM jobs WHERE policy=? GROUP BY status`, policy)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err = rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}

func (s *Store) Summaries(ctx context.Context, policy string, limit int) ([]domain.ActivitySummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,summary,created_at FROM activity_summaries WHERE policy=? ORDER BY id DESC LIMIT ?`, policy, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	summaries := []domain.ActivitySummary{}
	for rows.Next() {
		var summary domain.ActivitySummary
		var raw, created string
		var id int64
		if err = rows.Scan(&id, &raw, &created); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &summary); err != nil {
			return nil, err
		}
		summary.ID = id
		summary.CreatedAt = created
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}

func (s *Store) SaveSummary(ctx context.Context, summary domain.ActivitySummary) error {
	raw, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO activity_summaries(policy,to_event,summary)VALUES(?,?,?) ON CONFLICT(policy,to_event) DO NOTHING`, summary.Policy, summary.ToEvent, string(raw))
	return err
}

// Import the latest known outcomes from before the append-only activity log.
// Earlier retry transitions cannot be reconstructed from mutable job rows.
func (s *Store) backfillActivity() error {
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM jobs j WHERE status IN ('applied','failed','skipped','planned') AND error NOT LIKE 'superseded%' AND NOT EXISTS(SELECT 1 FROM activity a WHERE a.job_id=j.id AND a.kind IN `+visibleKinds+`) ORDER BY updated_at,id`)
	if err != nil {
		return err
	}
	var jobs []Job
	for rows.Next() {
		j, err := read(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, j := range jobs {
		e := eventForJob(j, j.Status, j.Error)
		e.CreatedAt = j.UpdatedAt
		if err = recordEvent(ctx, tx, e); err != nil {
			return err
		}
	}
	return tx.Commit()
}
