package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func TestManualReviewRetainsSkippedEvidenceAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err = s.Enqueue(ctx, []int{7}, "policy"); err != nil {
		t.Fatal(err)
	}
	j, err := s.Claim(ctx, time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CheckReview(ctx, "review", j.ID, 7); err == nil {
		t.Fatal("active job accepted for manual review")
	}
	p := domain.Proposal{Book: domain.Book{ID: 7}, Decision: domain.Decision{Reason: "No provider candidates"}, PolicyVersion: "policy"}
	if err = s.Proposal(ctx, j.ID, p); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, j.ID, "skipped", p.Decision.Reason, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckReview(ctx, "review", j.ID, 8); err == nil {
		t.Fatal("review accepted for an unrelated book")
	}
	event := domain.ActivityEvent{JobID: j.ID, BookID: 7, Policy: "policy", Kind: "applied", Stage: "manual-review", Details: domain.ActivityDetails{Reason: "Verified the title page", Sources: []string{"embedded title page"}, MovedFiles: 1}}
	for range 2 {
		if err = s.CheckReview(ctx, "review", j.ID, 7); err != nil {
			t.Fatal(err)
		}
		if err = s.Reconciled(ctx, "review", nil, event, ""); err != nil {
			t.Fatal(err)
		}
	}
	after, err := s.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "applied" || after.Error != "" || after.Proposal.Decision.Reason != p.Decision.Reason || after.Proposal.Decision.Match != 0 {
		t.Fatalf("manual review lost the original skip evidence: %+v", after)
	}
	var result map[string]any
	if err = json.Unmarshal(after.Result, &result); err != nil || result["manualReviewRun"] != "review" {
		t.Fatalf("missing manual provenance: %s %v", after.Result, err)
	}
	events, err := s.Activity(ctx, "policy", 0, -1)
	if err != nil || len(events) != 2 || events[0].Stage != "manual-review" || events[1].Kind != "skipped" {
		t.Fatalf("manual review should append one event and retain the skip: %+v %v", events, err)
	}
	if err = s.Reconciled(ctx, "different-plan", map[int]int{9: 7}, event, ""); err == nil {
		t.Fatal("an unrelated plan replaced completed review")
	}
	aliases, err := s.Aliases(ctx)
	if err != nil || len(aliases) != 0 {
		t.Fatalf("rejected manual review changed aliases: %v %v", aliases, err)
	}
}
