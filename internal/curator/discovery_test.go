package curator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func TestDiscoverUsesWholeInventoryWithoutSeriesKey(t *testing.T) {
	books := []domain.Book{
		{ID: 1, Metadata: domain.Metadata{Title: "The First Voyage", SeriesName: "Wrong"}, Files: []domain.File{{AbsolutePath: "/private/library/Writer/Voyages/First.epub"}}},
		{ID: 2, Metadata: domain.Metadata{Title: "The Second Voyage"}},
		{ID: 3, Metadata: domain.Metadata{Title: "An Unrelated Cookbook", SeriesName: "Wrong"}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string    `json:"model"`
			Messages []message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Model != "terra" || len(req.Messages) != 1 || strings.Contains(req.Messages[0].Content[1].Text, "/private/") {
			t.Error("bad discovery input")
		}
		var input []struct {
			ID        int      `json:"bookId"`
			Series    string   `json:"catalogSeriesHint"`
			FileHints []string `json:"fileHints"`
		}
		if err := json.Unmarshal([]byte(req.Messages[0].Content[1].Text), &input); err != nil {
			t.Error(err)
			return
		}
		if len(input) != 3 || input[1].Series != "" || input[0].FileHints[1] != "Voyages" {
			t.Error("inventory filtered or lost independent hints")
		}
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}]}`, `{"groups":[{"bookIds":[1,2],"reason":"Related voyages despite dirty series fields"},{"bookIds":[3],"reason":"Unrelated cookbook despite matching old series"}]}`)
	}))
	defer server.Close()
	plan, err := New(server.URL, "key", "haiku", "terra").Discover(context.Background(), books)
	if err != nil || len(plan.Groups) != 2 || len(plan.Groups[0].BookIDs) != 2 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
}

func TestDiscoveryKeepsRealRecordsAfterTyposAndOverlaps(t *testing.T) {
	groups, warnings, err := normalizeGroups([]domain.Group{{BookIDs: []int{1, 5701}, Reason: "first"}, {BookIDs: []int{1, 2}, Reason: "related"}}, []domain.Book{{ID: 1}, {ID: 2}, {ID: 3}})
	if err != nil || len(groups) != 2 || len(groups[0].BookIDs) != 2 || groups[1].BookIDs[0] != 3 || len(warnings) != 2 {
		t.Fatalf("groups: %+v warnings: %+v error: %v", groups, warnings, err)
	}
	for _, g := range groups {
		for _, id := range g.BookIDs {
			if id == 5701 {
				t.Fatal("invented ID survived")
			}
		}
	}
}
