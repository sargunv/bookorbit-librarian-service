package organize

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Set LIBRARIAN_TEST_DATABASE_URL to an EMPTY disposable database whose name
// starts with librarian_test_. This exercises real FK cascades and SQL conflicts.
func TestReferencePreservationIntegration(t *testing.T) {
	dsn := os.Getenv("LIBRARIAN_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("disposable PostgreSQL database not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "librarian_test_") {
		t.Fatal("integration tests require a disposable librarian_test_ database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	_, err = db.ExecContext(ctx,
		`CREATE TABLE books(id integer PRIMARY KEY);
	 CREATE TABLE user_book_status(user_id integer,book_id integer REFERENCES books ON DELETE CASCADE,status text,source text,updated_at timestamptz,PRIMARY KEY(user_id,book_id));
	 CREATE TABLE user_book_ratings(user_id integer,book_id integer REFERENCES books ON DELETE CASCADE,rating integer,updated_at timestamptz,PRIMARY KEY(user_id,book_id));
	 CREATE TABLE collections(id integer PRIMARY KEY);
	 INSERT INTO collections VALUES(1),(2);
	 CREATE TABLE collection_books(collection_id integer,book_id integer REFERENCES books ON DELETE CASCADE,PRIMARY KEY(collection_id,book_id));
	 CREATE TABLE bookmarks(id integer PRIMARY KEY,book_id integer REFERENCES books ON DELETE CASCADE,note text);
	 CREATE TABLE bookmark_children(id integer PRIMARY KEY,parent integer REFERENCES bookmarks ON DELETE CASCADE);
	 CREATE TABLE future_state(id integer PRIMARY KEY,book_id integer REFERENCES books ON DELETE CASCADE,device text,UNIQUE(book_id,device));
	 INSERT INTO books VALUES(1),(2);
	 INSERT INTO user_book_status VALUES(1,1,'to_read','user','2025-01-01'),(1,2,'read','user','2025-01-02');
	 INSERT INTO user_book_ratings VALUES(1,1,90,'2025-01-03'),(1,2,80,'2025-01-02'),(2,2,70,'2025-01-02');
	 INSERT INTO collection_books VALUES(1,1),(1,2),(2,2);
	 INSERT INTO bookmarks VALUES(10,2,'original annotation');
	 INSERT INTO bookmark_children VALUES(20,10);
	 INSERT INTO future_state VALUES(1,2,'device');`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, "DROP TABLE bookmark_children,bookmarks,user_book_status,user_book_ratings,collection_books,collections,future_state,books CASCADE")
	}()
	o := &Organizer{originals: filepath.Join(t.TempDir(), "originals"), db: db}
	if err = os.MkdirAll(o.originals, 0o700); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = o.transferReferences(ctx, tx, 2, 1); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM books WHERE id=2"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var status string
	var rating, count int
	if err = db.QueryRowContext(ctx, "SELECT status FROM user_book_status WHERE user_id=1 AND book_id=1").Scan(&status); err != nil || status != "read" {
		t.Fatalf("lost reading status: %s %v", status, err)
	}
	if err = db.QueryRowContext(ctx, "SELECT rating FROM user_book_ratings WHERE user_id=1 AND book_id=1").Scan(&rating); err != nil || rating != 90 {
		t.Fatalf("overwrote newer rating: %d %v", rating, err)
	}
	for _, q := range []string{"SELECT count(*) FROM collection_books WHERE book_id=1", "SELECT count(*) FROM user_book_ratings WHERE book_id=1"} {
		if err = db.QueryRowContext(ctx, q).Scan(&count); err != nil || count != 2 {
			t.Fatalf("lost memberships/ratings: %d %v", count, err)
		}
	}
	for _, q := range []string{"SELECT count(*) FROM bookmarks b JOIN bookmark_children c ON c.parent=b.id WHERE b.book_id=1", "SELECT count(*) FROM future_state WHERE book_id=1"} {
		if err = db.QueryRowContext(ctx, q).Scan(&count); err != nil || count != 1 {
			t.Fatalf("lost referenced children/future state: %d %v", count, err)
		}
	}
	paths, _ := filepath.Glob(filepath.Join(o.originals, "merges", "*.json"))
	if len(paths) != 1 {
		t.Fatal("reference evidence was not archived")
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err = json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	// Recovery of an older snapshot is safe to repeat and cannot roll back newer values.
	for range 2 {
		if _, err = o.RestoreReferences(ctx, snapshot, map[int]int{2: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.QueryRowContext(ctx, "SELECT rating FROM user_book_ratings WHERE user_id=1 AND book_id=1").Scan(&rating); err != nil || rating != 90 {
		t.Fatal("recovery overwrote newer user data")
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO books VALUES(2);INSERT INTO future_state VALUES(2,2,'device')"); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = o.transferReferences(ctx, tx, 2, 1); err == nil {
		_ = tx.Rollback()
		t.Fatal("unknown unique conflict did not fail closed")
	}
	_ = tx.Rollback()
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM future_state WHERE book_id=2").Scan(&count); err != nil || count != 1 {
		t.Fatal("conflicting state was lost")
	}
}

// Separately exercises an operator's real backup on a disposable COPY of the
// application database. Private inputs stay outside the repository and stdout.
func TestBackupRecoveryIntegration(t *testing.T) {
	dsn, snapshotPath, aliasesPath := os.Getenv("LIBRARIAN_TEST_DATABASE_URL"), os.Getenv("LIBRARIAN_TEST_RECOVERY_SNAPSHOT"), os.Getenv("LIBRARIAN_TEST_ALIASES")
	if dsn == "" || snapshotPath == "" || aliasesPath == "" {
		t.Skip("private recovery rehearsal not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "librarian_test_") {
		t.Fatal("rehearsal requires a disposable database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err = json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(aliasesPath)
	if err != nil {
		t.Fatal(err)
	}
	var aliases map[int]int
	if err = json.Unmarshal(data, &aliases); err != nil {
		t.Fatal(err)
	}
	o := &Organizer{db: db}
	for range 2 {
		if _, err = o.RestoreReferences(context.Background(), snapshot, aliases); err != nil {
			t.Fatal(err)
		}
	}
}
