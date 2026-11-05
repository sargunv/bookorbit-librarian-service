package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sargunv/bookorbit-librarian-service/internal/curator"
	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
	"github.com/sargunv/bookorbit-librarian-service/internal/store"
)

func (s *Service) progress(ctx context.Context, j store.Job, title, stage string, done, total int) {
	message := map[string]string{"started": "Opening book record", "grouping": "Finding related books", "sources": "Searching metadata sources", "covers": "Gathering covers", "vision": "Comparing cover images", "metadata": "Choosing metadata and covers", "applying": "Writing selected metadata", "organizing": "Organizing book files"}[stage]
	if total > 0 {
		switch stage {
		case "vision":
			message = fmt.Sprintf("Comparing covers: %d of %d images", done, total)
		case "metadata":
			message = fmt.Sprintf("Choosing metadata and covers for %d books", total)
		default:
			message = fmt.Sprintf("%s: %d of %d books", message, done, total)
		}
	}
	if err := s.db.RecordEvent(ctx, domain.ActivityEvent{JobID: j.ID, BookID: j.BookID, Policy: j.Policy, Kind: "progress", Stage: stage, Title: title, Message: message}); err != nil {
		slog.Warn("activity progress persistence failed", "job", j.ID)
	}
}

func (s *Service) summarizeActivity(ctx context.Context) {
	for {
		err := s.updateActivitySummary(ctx)
		if ctx.Err() != nil {
			return
		}
		s.summaryMu.Lock()
		if err != nil {
			s.summaryError = "The summary could not be updated. The activity entries below are current."
		} else {
			s.summaryError = ""
		}
		s.summaryMu.Unlock()
		if err != nil {
			slog.Warn("activity summary update failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.SummaryInterval):
		}
	}
}

func (s *Service) updateActivitySummary(ctx context.Context) error {
	summaries, err := s.db.Summaries(ctx, curator.PolicyVersion, 1)
	if err != nil {
		return err
	}
	var after int64
	if len(summaries) > 0 {
		after = summaries[0].ToEvent
	}
	events, err := s.db.ActivityAfter(ctx, curator.PolicyVersion, after, 60)
	if err != nil || len(events) == 0 {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	summary, err := s.judge.Summarize(bounded, events, s.cfg.SummaryModel)
	if err != nil {
		return err
	}
	summary.ThroughAt = events[len(events)-1].CreatedAt
	summary.Policy = curator.PolicyVersion
	summary.FromEvent = events[0].ID
	summary.ToEvent = events[len(events)-1].ID
	return s.db.SaveSummary(ctx, summary)
}
