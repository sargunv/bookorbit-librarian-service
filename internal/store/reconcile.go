package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func (s *Store) AllJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+columns+" FROM jobs ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var jobs []Job
	for rows.Next() {
		j, err := read(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *Store) Aliases(ctx context.Context) (map[int]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT old_id,book_id FROM book_aliases")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	aliases := map[int]int{}
	for rows.Next() {
		var old, dest int
		if err = rows.Scan(&old, &dest); err != nil {
			return nil, err
		}
		aliases[old] = dest
	}
	return aliases, rows.Err()
}

func (s *Store) ResolveBook(ctx context.Context, id int) (int, error) {
	aliases, err := s.Aliases(ctx)
	if err != nil {
		return 0, err
	}
	seen := map[int]bool{}
	for aliases[id] != 0 {
		if seen[id] {
			return 0, fmt.Errorf("cyclic book alias %d", id)
		}
		seen[id] = true
		id = aliases[id]
	}
	return id, nil
}

func (s *Store) ReconciliationDone(ctx context.Context, run string, book int) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM reconciliations WHERE run=? AND book_id=?", run, book).Scan(&n)
	return n > 0, err
}

func reviewable(j Job, run string, book int) error {
	var result struct {
		Run string `json:"manualReviewRun"`
	}
	_ = json.Unmarshal(j.Result, &result)
	allowed := j.Status == "skipped" || (j.Status == "applied" && result.Run == run)
	if run == "" || j.BookID != book || !allowed {
		return fmt.Errorf("manual review requires the skipped job for book %d or its same-plan retry", book)
	}
	return nil
}

// CheckReview rejects active or unrelated jobs before any external side effects.
func (s *Store) CheckReview(ctx context.Context, run string, job int64, book int) error {
	j, err := s.Get(ctx, job)
	if err != nil {
		return err
	}
	return reviewable(j, run, book)
}

// Reconciled records verification and aliases with its activity event in one
// SQLite transaction. The plan's run key makes interrupted retries idempotent.
func (s *Store) Reconciled(ctx context.Context, run string, aliases map[int]int, event domain.ActivityEvent, coverURL string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if event.Stage == "manual-review" {
		j, e := read(tx.QueryRowContext(ctx, "SELECT "+columns+" FROM jobs WHERE id=?", event.JobID))
		if e != nil {
			return e
		}
		if e = reviewable(j, run, event.BookID); e != nil {
			return e
		}
		result, e := json.Marshal(map[string]any{"manualReviewRun": run, "reason": event.Details.Reason, "sources": event.Details.Sources, "movedFileCount": event.Details.MovedFiles})
		if e != nil {
			return e
		}
		// Keep the original skipped proposal and append-only history as evidence.
		if j.Status == "skipped" {
			if _, err = tx.ExecContext(ctx, `UPDATE jobs SET status='applied',error='',result=?,lease_until=0,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, string(result), event.JobID); err != nil {
				return err
			}
		}
	}
	for old, dest := range aliases {
		if _, err = tx.ExecContext(ctx, "INSERT INTO book_aliases(old_id,book_id) VALUES(?,?) ON CONFLICT(old_id) DO UPDATE SET book_id=excluded.book_id", old, dest); err != nil {
			return err
		}
	}
	if coverURL != "" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO reconciled_covers(book_id,url) VALUES(?,?) ON CONFLICT(book_id) DO UPDATE SET url=excluded.url", event.BookID, coverURL); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(event)
	r, err := tx.ExecContext(ctx, "INSERT INTO reconciliations(run,book_id,event) VALUES(?,?,?) ON CONFLICT DO NOTHING", run, event.BookID, string(raw))
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 && event.JobID != 0 {
		if err = recordEvent(ctx, tx, event); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ReconciledCover(ctx context.Context, id int, url string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM reconciled_covers WHERE book_id=? AND url=?", id, url).Scan(&n)
	return n > 0, err
}
