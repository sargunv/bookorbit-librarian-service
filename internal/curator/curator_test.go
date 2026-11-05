package curator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func TestSourcesEditionAndLocks(t *testing.T) {
	book := domain.Book{LockedFields: []string{"title", "cover"}, Metadata: domain.Metadata{Title: "Keep", ISBN13: "original"}}
	candidates := []domain.Candidate{
		{Provider: "hardcover", ProviderID: "work", HardcoverEditionID: "edition", Metadata: domain.Metadata{Title: "New", Description: "Short", ISBN13: "wrong edition"}, CoverURL: "https://example.com/a.jpg"},
		{Provider: "goodreads", ProviderID: "other", Metadata: domain.Metadata{Description: "<p>First &amp; true.</p><p>Second.</p>", ISBN13: "right edition"}},
	}
	decision := domain.Decision{Match: 1, WorkSources: []int{1, 2}, EditionSources: []int{2}, CoverSource: 1, FieldSources: map[string]int{"title": 1, "description": 2, "isbn13": 2}}
	p, err := BuildProposal(book, candidates, decision)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Patch["title"]; ok || p.CoverURL != "" {
		t.Fatal("locked field changed")
	}
	if string(p.Patch["description"]) != `"First \u0026 true.\n\nSecond."` || p.Provenance["description"] != "goodreads/other" {
		t.Fatalf("description: %s %+v", p.Patch["description"], p.Provenance)
	}
	if string(p.Patch["isbn13"]) != `"right edition"` {
		t.Fatal("wrong edition value")
	}
	if _, ok := p.Patch["hardcoverEditionId"]; ok {
		t.Fatal("work match supplied edition ID")
	}
	decision.FieldSources["isbn13"] = 1
	if _, err = BuildProposal(book, candidates, decision); err == nil {
		t.Fatal("accepted unsupported edition field")
	}
	decision.FieldSources = map[string]int{"description": 2}
	decision.WorkSources = []int{1}
	decision.EditionSources = nil
	if _, err = BuildProposal(book, candidates, decision); err == nil {
		t.Fatal("accepted nonmatching source")
	}
}

func TestPreserveEmptyAndUnchanged(t *testing.T) {
	b := domain.Book{Metadata: domain.Metadata{Title: "Same", Description: "Good"}, ProviderIDs: map[string]string{"hardcover": "a"}}
	cs := []domain.Candidate{{Provider: "hardcover", ProviderID: "a", Metadata: domain.Metadata{Title: "Same"}}}
	p, err := BuildProposal(b, cs, domain.Decision{Match: 1, WorkSources: []int{1}, FieldSources: map[string]int{"title": 1, "description": 1}})
	if err != nil || len(p.Patch) != 0 {
		t.Fatalf("empty or unchanged values patched: %+v %v", p, err)
	}
}

func TestSeriesHasFixedVisualAndEditorialTasks(t *testing.T) {
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 20, 30))); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cover" {
			_, _ = w.Write(pngData.Bytes())
			return
		}
		calls++
		var req struct {
			Model    string    `json:"model"`
			Messages []message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Messages[0].Role != "developer" {
			t.Error("proxy instruction role must be developer")
		}
		content := ""
		switch calls {
		case 1:
			if req.Model != "haiku" || len(req.Messages[1].Content) != 2 || req.Messages[1].Content[1].ImageURL == nil {
				t.Error("vision did not receive contact sheet")
			}
			content = `{"summary":"matching blue set", "covers":[{"bookId":1,"source":0,"family":"blue","visibleText":"One","issues":[]},{"bookId":1,"source":1,"family":"blue","visibleText":"One","issues":[]},{"bookId":2,"source":0,"family":"blue","visibleText":"Two","issues":[]},{"bookId":2,"source":1,"family":"blue","visibleText":"Two","issues":[]}]}`
		case 2:
			input := req.Messages[1].Content[0].Text
			if req.Model != "terra" || !strings.Contains(input, "matching blue set") || strings.Contains(input, "/private/") {
				t.Error("editorial input missing observations or contains private paths")
			}
			content = `{"reason":"retain the coherent set", "books":[{"bookId":1,"match":1,"workSources":[1],"editionSources":[],"fieldSources":{"description":1},"cover":0,"reason":"same work"},{"bookId":2,"match":1,"workSources":[1],"editionSources":[],"fieldSources":{"description":1},"cover":0,"reason":"same work"}]}`
		default:
			t.Error("unexpected extra model call")
		}
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":7}}`, content)
	}))
	defer server.Close()
	evidence := []domain.Evidence{}
	for _, id := range []int{1, 2} {
		evidence = append(evidence, domain.Evidence{Book: domain.Book{ID: id, Files: []domain.File{{AbsolutePath: "/private/book.epub"}}}, Candidates: []domain.Candidate{{Provider: "hardcover", ProviderID: "a", Metadata: domain.Metadata{Description: "Evidence"}, CoverURL: server.URL + "/cover"}}})
	}
	ps, err := New(server.URL, "key", "haiku", "terra").Plan(context.Background(), evidence, func(context.Context, int) ([]byte, error) { return pngData.Bytes(), nil })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(ps) != 2 || ps[0].SeriesReason != ps[1].SeriesReason || ps[0].Visual.Model != "haiku" || ps[0].Decision.Model != "terra" {
		t.Fatalf("unexpected plan %+v", ps)
	}
	if ps[0].Decision.InputTokens != 12 || ps[1].Decision.InputTokens != 0 {
		t.Fatal("series usage multiplied")
	}
}

func TestRejectUnseenCover(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"reason\":\"bad\",\"books\":[{\"bookId\":1,\"match\":1,\"workSources\":[1],\"editionSources\":[],\"fieldSources\":{},\"cover\":1,\"reason\":\"bad\"}]}"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	_, err := New(server.URL, "secret", "haiku", "terra").Plan(context.Background(), []domain.Evidence{{Book: domain.Book{ID: 1}, Candidates: []domain.Candidate{{Provider: "hardcover"}}}}, func(context.Context, int) ([]byte, error) { return nil, fmt.Errorf("no cover") })
	if err == nil || !strings.Contains(err.Error(), "unseen cover") {
		t.Fatalf("expected unseen cover rejection: %v", err)
	}
}
