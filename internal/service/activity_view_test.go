package service

import (
	"encoding/json"
	"testing"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func TestActivityImpactSeparatesLinksFromBibliographicAndCoverChanges(t *testing.T) {
	change := func(field string) domain.FieldChange {
		return domain.FieldChange{Field: field, Before: json.RawMessage(`"old"`), After: json.RawMessage(`"new"`), Source: "goodreads/123"}
	}
	for _, tc := range []struct {
		name    string
		details domain.ActivityDetails
		impact  string
	}{
		{"many links", domain.ActivityDetails{Changes: []domain.FieldChange{change("hardcoverId"), change("hardcoverEditionId"), change("goodreadsId")}}, "links"},
		{"description only", domain.ActivityDetails{Changes: []domain.FieldChange{change("description")}}, "metadata"},
		{"description and cover", domain.ActivityDetails{CoverChanged: true, Changes: []domain.FieldChange{change("description")}}, "major"},
		{"three fields", domain.ActivityDetails{Changes: []domain.FieldChange{change("publisher"), change("publishedDate"), change("pageCount")}}, "major"},
		{"cover only", domain.ActivityDetails{CoverChanged: true}, "cover"},
		{"files only", domain.ActivityDetails{MovedFiles: 1}, "files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := activityView(domain.ActivityEvent{Kind: "applied", Details: tc.details})
			if r.Impact != tc.impact {
				t.Fatalf("got %s", r.Impact)
			}
		})
	}
}
