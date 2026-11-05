package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func TestActivityPreservesRetryAfterSuccess(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err = s.Enqueue(ctx, []int{1}, "test"); err != nil {
		t.Fatal(err)
	}
	j, err := s.Claim(ctx, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordEvent(ctx, domain.ActivityEvent{JobID: j.ID, BookID: 1, Policy: "test", Kind: "progress", Stage: "vision", Title: "A Book", Message: "Comparing covers"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Fail(ctx, j, "vision returned an unknown cover", 3); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, j.ID, "applied", "", nil); err != nil {
		t.Fatal(err)
	}
	events, err := s.Activity(ctx, "test", 0, 30)
	if err != nil || len(events) != 2 || events[0].Kind != "applied" || events[1].Kind != "retry" || events[1].Details.Error != "vision returned an unknown cover" || events[0].Title != "A Book" {
		t.Fatalf("history: %+v %v", events, err)
	}
	active, err := s.ActiveActivity(ctx, "test")
	if err != nil || len(active) != 0 {
		t.Fatalf("finished book appears active: %+v %v", active, err)
	}
}

func TestSummaryAndHistoricalActivitySurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO jobs(book_id,policy,status)VALUES(1,'test','applied')`); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveSummary(ctx, domain.ActivitySummary{Policy: "test", ToEvent: 1, Highlights: []domain.SummaryHighlight{{Text: "A book changed.", EventIDs: []int64{1}}}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	events, err := s.Activity(ctx, "test", 0, 30)
	if err != nil || len(events) != 1 {
		t.Fatalf("backfill: %+v %v", events, err)
	}
	summaries, err := s.Summaries(ctx, "test", 1)
	if err != nil || len(summaries) != 1 || summaries[0].ID == 0 || summaries[0].CreatedAt == "" || summaries[0].ToEvent != 1 {
		t.Fatalf("saved summary: %+v %v", summaries, err)
	}
}

func TestHistoricalCoverageUsesSavedProposalWithoutRewritingEvents(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err = s.Enqueue(ctx, []int{1}, "test"); err != nil {
		t.Fatal(err)
	}
	j, err := s.Claim(ctx, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	p := domain.Proposal{Book: domain.Book{Metadata: domain.Metadata{Title: "A Book"}}, Patch: domain.Patch{"description": []byte(`"Added description"`)}}
	if err = s.Proposal(ctx, j.ID, p); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, j.ID, "applied", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE activity SET details=json_remove(details,'$.coverage')`); err != nil {
		t.Fatal(err)
	}
	events, err := s.Activity(ctx, "test", 0, 30)
	if err != nil || len(events) != 1 {
		t.Fatalf("events: %v %v", events, err)
	}
	c := events[0].Details.Coverage
	if c == nil || c.Before != 1 || c.After != 2 {
		t.Fatalf("historical coverage: %+v", c)
	}
}
