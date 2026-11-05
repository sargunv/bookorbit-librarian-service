package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
	"github.com/sargunv/bookorbit-librarian-service/internal/reconcile"
)

func TestManualReviewRejectsUnsupportedPlanBeforeExternalWrites(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ids   []int
		patch domain.Patch
		want  string
	}{
		{"merge", []int{7, 8}, nil, "single surviving book"},
		{"wrong survivor", []int{8}, nil, "single surviving book"},
		{"missing provenance", []int{7}, domain.Patch{"publisher": json.RawMessage(`"Example Press"`)}, "lacks evidence provenance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Nil clients would panic if validation allowed any external access.
			s := &Service{}
			err := s.applyReconciliation(context.Background(), "review", nil, reconcile.Group{Survivor: 7, BookIDs: tc.ids, JobID: 1, Patch: tc.patch, ReviewReason: "Reviewed the title page"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsupported review was not rejected before writes: %v", err)
			}
		})
	}
}
