package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func TestLeaseRecoveryRetainsProposalAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if n, e := s.Enqueue(ctx, []int{7, 7}, "policy"); e != nil || n != 1 {
		t.Fatalf("enqueue: %d %v", n, e)
	}
	j, e := s.Claim(ctx, time.Minute, 3)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Claim(ctx, time.Minute, 3); !errors.Is(e, sql.ErrNoRows) {
		t.Fatalf("double claim: %v", e)
	}
	p := domain.Proposal{Book: domain.Book{ID: 7}, PolicyVersion: "policy"}
	if e = s.Proposal(ctx, j.ID, p); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec(`UPDATE jobs SET lease_until=0`); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.Close() }()
	recovered, e := s.Claim(ctx, time.Minute, 3)
	if e != nil {
		t.Fatal(e)
	}
	if recovered.ID != j.ID || recovered.Attempts != 2 || recovered.Proposal == nil || recovered.Proposal.Book.ID != 7 {
		t.Fatalf("bad recovered job: %+v", recovered)
	}
	if e = s.Finish(ctx, j.ID, "applied", "", nil); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Claim(ctx, time.Minute, 3); !errors.Is(e, sql.ErrNoRows) {
		t.Fatalf("completed job reclaimed: %v", e)
	}
}

func TestRetryExhaustionAndExplicitRequeue(t *testing.T) {
	ctx := context.Background()
	s, e := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.Close() }()
	if _, e = s.Enqueue(ctx, []int{1}, "p"); e != nil {
		t.Fatal(e)
	}
	j, e := s.Claim(ctx, time.Minute, 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Fail(ctx, j, "source unavailable", 1); e != nil {
		t.Fatal(e)
	}
	got, e := s.Get(ctx, j.ID)
	if e != nil || got.Status != "failed" {
		t.Fatalf("unexpected failure state: %+v %v", got, e)
	}
	if e = s.Retry(ctx, j.ID); e != nil {
		t.Fatal(e)
	}
	got, e = s.Claim(ctx, time.Minute, 1)
	if e != nil || got.Attempts != 1 {
		t.Fatalf("explicit retry: %+v %v", got, e)
	}
	if e = s.Retry(ctx, j.ID); e == nil {
		t.Fatal("active job was reset")
	}
}
