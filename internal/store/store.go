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
 CREATE INDEX IF NOT EXISTS jobs_claim ON jobs(status,available_at,id);`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
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
	var r sql.NullString
	if len(result) > 0 {
		r = sql.NullString{String: string(result), Valid: true}
	}
	_, e := s.db.ExecContext(ctx, `UPDATE jobs SET status=?,error=?,result=?,lease_until=0,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, status, message, r, id)
	return e
}

func (s *Store) Fail(ctx context.Context, j Job, message string, maxAttempts int) error {
	status := "pending"
	if j.Attempts >= maxAttempts {
		status = "failed"
	}
	_, e := s.db.ExecContext(ctx, `UPDATE jobs SET status=?,error=?,available_at=?,lease_until=0,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, status, message, time.Now().Add(time.Duration(j.Attempts*j.Attempts)*time.Minute).Unix(), j.ID)
	return e
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
