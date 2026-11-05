package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
	_ "modernc.org/sqlite"
)

type (
	Store struct{ db *sql.DB }
	Job   struct {
		ID        int64            `json:"id"`
		BookID    int              `json:"bookId"`
		Status    string           `json:"status"`
		Attempts  int              `json:"attempts"`
		Policy    string           `json:"policyVersion"`
		Error     string           `json:"error,omitempty"`
		Proposal  *domain.Proposal `json:"proposal,omitempty"`
		Result    json.RawMessage  `json:"result,omitempty"`
		CreatedAt string           `json:"createdAt"`
		UpdatedAt string           `json:"updatedAt"`
	}
)

const columns = `id,book_id,status,attempts,policy,error,proposal,result,created_at,updated_at`

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA synchronous=FULL;
 CREATE TABLE IF NOT EXISTS jobs(id INTEGER PRIMARY KEY,book_id INTEGER NOT NULL,policy TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'pending',attempts INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',proposal TEXT,result TEXT,lease_until INTEGER NOT NULL DEFAULT 0,available_at INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),UNIQUE(book_id,policy));
 CREATE TABLE IF NOT EXISTS activity(id INTEGER PRIMARY KEY,job_id INTEGER NOT NULL,book_id INTEGER NOT NULL,policy TEXT NOT NULL,kind TEXT NOT NULL,stage TEXT NOT NULL DEFAULT '',title TEXT NOT NULL DEFAULT '',message TEXT NOT NULL DEFAULT '',details TEXT NOT NULL DEFAULT '{}',created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')));
 CREATE INDEX IF NOT EXISTS activity_policy ON activity(policy,id);
 CREATE INDEX IF NOT EXISTS activity_job ON activity(job_id,id);
 CREATE TABLE IF NOT EXISTS activity_summaries(id INTEGER PRIMARY KEY,policy TEXT NOT NULL,to_event INTEGER NOT NULL,summary TEXT NOT NULL,created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),UNIQUE(policy,to_event));
 CREATE TABLE IF NOT EXISTS discovery(policy TEXT PRIMARY KEY,plan TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS jobs_claim ON jobs(status,available_at,id);`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = s.backfillActivity(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *Store) Enqueue(ctx context.Context, ids []int, policy string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	n := 0
	for _, id := range ids {
		if id < 1 {
			continue
		}
		r, e := tx.ExecContext(ctx, `INSERT INTO jobs(book_id,policy)VALUES(?,?) ON CONFLICT DO NOTHING`, id, policy)
		if e != nil {
			return 0, e
		}
		v, e := r.RowsAffected()
		if e != nil {
			return 0, e
		}
		n += int(v)
	}
	return n, tx.Commit()
}

type scanner interface{ Scan(...interface{}) error }

func read(row scanner) (Job, error) {
	var j Job
	var proposal, result sql.NullString
	err := row.Scan(&j.ID, &j.BookID, &j.Status, &j.Attempts, &j.Policy, &j.Error, &proposal, &result, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return j, err
	}
	if proposal.Valid {
		var p domain.Proposal
		if e := json.Unmarshal([]byte(proposal.String), &p); e != nil {
			return j, e
		}
		j.Proposal = &p
	}
	if result.Valid {
		j.Result = json.RawMessage(result.String)
	}
	return j, nil
}

func (s *Store) Claim(ctx context.Context, lease time.Duration, maxAttempts int) (Job, error) {
	now := time.Now().Unix()
	// Expired leases are retried with their persisted proposal; unclaimed exhausted jobs fail visibly.
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET status=CASE WHEN attempts>=? THEN 'failed' ELSE 'pending' END,error='worker lease expired' WHERE status='running' AND lease_until<?`, maxAttempts, now)
	if err != nil {
		return Job{}, err
	}
	return read(s.db.QueryRowContext(ctx, `UPDATE jobs SET status='running',attempts=attempts+1,lease_until=?,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=(SELECT id FROM jobs WHERE status='pending' AND available_at<=? AND attempts<? ORDER BY id LIMIT 1) RETURNING `+columns, now+int64(lease.Seconds()), now, maxAttempts))
}

func (s *Store) Proposal(ctx context.Context, id int64, p domain.Proposal) error {
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, `UPDATE jobs SET proposal=?,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, string(b), id)
	return e
}

func (s *Store) Finish(ctx context.Context, id int64, status, message string, result json.RawMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var raw sql.NullString
	if len(result) > 0 {
		raw = sql.NullString{String: string(result), Valid: true}
	}
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET status=?,error=?,result=?,lease_until=0,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, status, message, raw, id)
	if err != nil {
		return err
	}
	j, err := read(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM jobs WHERE id=?`, id))
	if err != nil {
		return err
	}
	event := eventForJob(j, status, message)
	if event.Title == "" {
		_ = tx.QueryRowContext(ctx, `SELECT title FROM activity WHERE job_id=? AND title<>'' ORDER BY id DESC LIMIT 1`, id).Scan(&event.Title)
	}
	if err = recordEvent(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Fail(ctx context.Context, j Job, message string, maxAttempts int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	status, kind := "pending", "retry"
	if j.Attempts >= maxAttempts {
		status, kind = "failed", "failed"
	}
	_, err = tx.ExecContext(ctx, `UPDATE jobs SET status=?,error=?,available_at=?,lease_until=0,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, status, message, time.Now().Add(time.Duration(j.Attempts*j.Attempts)*time.Minute).Unix(), j.ID)
	if err != nil {
		return err
	}
	saved, err := read(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM jobs WHERE id=?`, j.ID))
	if err != nil {
		return err
	}
	event := eventForJob(saved, kind, message)
	if event.Title == "" {
		_ = tx.QueryRowContext(ctx, `SELECT title FROM activity WHERE job_id=? AND title<>'' ORDER BY id DESC LIMIT 1`, j.ID).Scan(&event.Title)
	}
	if err = recordEvent(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Retry(ctx context.Context, id int64) error {
	r, e := s.db.ExecContext(ctx, `UPDATE jobs SET status='pending',attempts=0,error='',proposal=NULL,result=NULL,available_at=0 WHERE id=? AND status IN ('failed','review','planned')`, id)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("job not found or not retryable")
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id int64) (Job, error) {
	return read(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM jobs WHERE id=?`, id))
}

func (s *Store) List(ctx context.Context, status string) ([]Job, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT `+columns+` FROM jobs WHERE (?='' OR status=?) ORDER BY id DESC LIMIT 100`, status, status)
	if e != nil {
		return nil, e
	}
	defer func() { _ = rows.Close() }()
	out := []Job{}
	for rows.Next() {
		j, e := read(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT status,count(*) FROM jobs GROUP BY status`)
	if e != nil {
		return nil, e
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if e := rows.Scan(&k, &n); e != nil {
			return nil, e
		}
		out[k] = n
	}
	return out, rows.Err()
}

// SaveGroup commits the whole series plan before any volume is edited. Sibling
// jobs reuse it, so individual volumes cannot independently choose cover sets.
func (s *Store) SaveGroup(ctx context.Context, proposals []domain.Proposal) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, p := range proposals {
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO jobs(book_id,policy,proposal) VALUES(?,?,?) ON CONFLICT(book_id,policy) DO NOTHING`, p.Book.ID, p.PolicyVersion, string(data)); err != nil {
			return err
		}
		// A new volume can justify changing an already curated sibling's cover.
		changed := len(p.Patch) > 0 || p.CoverURL != ""
		if _, err = tx.ExecContext(ctx, `UPDATE jobs SET proposal=?,attempts=CASE WHEN status='applied' AND ? THEN 0 ELSE attempts END,status=CASE WHEN status='applied' AND ? THEN 'pending' ELSE status END,available_at=0,error='',updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE book_id=? AND policy=? AND (proposal IS NULL OR status='applied')`, string(data), changed, changed, p.Book.ID, p.PolicyVersion); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Discovery(ctx context.Context, policy string) (domain.Discovery, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT plan FROM discovery WHERE policy=?`, policy).Scan(&raw)
	if err != nil {
		return domain.Discovery{}, err
	}
	var plan domain.Discovery
	err = json.Unmarshal([]byte(raw), &plan)
	return plan, err
}

func (s *Store) SaveDiscovery(ctx context.Context, policy string, plan domain.Discovery) error {
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO discovery(policy,plan) VALUES(?,?) ON CONFLICT(policy) DO UPDATE SET plan=excluded.plan`, policy, string(raw))
	return err
}
