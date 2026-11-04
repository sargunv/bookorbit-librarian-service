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

func TestBuildProposalEvidenceAndLocks(t *testing.T) {
	book := domain.Book{LockedFields: []string{"title", "cover"}, Metadata: domain.Metadata{ISBN13: "original"}}
	cs := []domain.Candidate{{Metadata: domain.Metadata{Title: "Evidence", Description: "<p>Real &amp; true</p>\x00", Authors: []string{"Writer"}, ISBN13: "edition", Publisher: "Press"}, Provider: "hardcover", ProviderID: "abc", HardcoverEditionID: "ed", CoverURL: "https://example.com/cover"}}
	d := domain.Decision{Match: 1, SameWork: true, SameEdition: true, FieldSources: map[string]int{"title": 1, "description": 1, "authors": 1, "isbn13": 1, "publisher": 1, "cover": 1}}
	p, err := BuildProposal(book, cs, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Patch["title"]; ok || p.CoverURL != "" {
		t.Fatal("locked fields changed")
	}
	for k, want := range map[string]string{"description": `"Real & true"`, "authors": `["Writer"]`, "isbn13": `"edition"`, "hardcoverId": `"abc"`, "hardcoverEditionId": `"ed"`} {
		canonical, err := json.Marshal(json.RawMessage(want))
		if err != nil {
			t.Fatal(err)
		}
		if string(p.Patch[k]) != string(canonical) {
			t.Errorf("%s = %s, want %s", k, p.Patch[k], want)
		}
		if p.Provenance[k] != "hardcover/abc" {
			t.Errorf("missing provenance for %s", k)
		}
	}
	d.SameEdition = false
	delete(d.FieldSources, "isbn13")
	delete(d.FieldSources, "publisher")
	p, err = BuildProposal(book, cs, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Patch["isbn13"]; ok {
		t.Fatal("work match changed ISBN")
	}
	if _, ok := p.Patch["hardcoverEditionId"]; ok {
		t.Fatal("work match changed edition ID")
	}
	if _, ok := p.Patch["description"]; !ok {
		t.Fatal("work evidence omitted")
	}
}

func TestIdentityGates(t *testing.T) {
	cs := []domain.Candidate{{Metadata: domain.Metadata{ISBN13: "x"}}}
	cases := []domain.Decision{
		{Match: 2, SameWork: true, FieldSources: map[string]int{}},
		{Match: 0, SameWork: true, FieldSources: map[string]int{}},
		{Match: 1, SameEdition: true, FieldSources: map[string]int{}},
		{Match: 1, SameWork: true, FieldSources: map[string]int{"bogus": 1}},
		{Match: 1, SameWork: true, FieldSources: map[string]int{"isbn13": 1}},
		{Match: 1, SameWork: true, FieldSources: map[string]int{"title": 2}},
	}
	for _, d := range cases {
		if _, err := BuildProposal(domain.Book{}, cs, d); err == nil {
			t.Errorf("accepted %+v", d)
		}
	}
	for _, d := range []domain.Decision{{Match: 0, FieldSources: map[string]int{}}, {Match: 1, SameWork: true, NeedsReview: true, FieldSources: map[string]int{"description": 1}}} {
		p, err := BuildProposal(domain.Book{}, cs, d)
		if err != nil || len(p.Patch) != 0 || p.CoverURL != "" {
			t.Fatalf("review/no match: %+v %v", p, err)
		}
	}
}

func TestJudgeHTTPFallbackAndUntrustedInput(t *testing.T) {
	count := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("request path/auth invalid")
		}
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(req.Messages[1].Content, "/private/") || strings.Contains(req.Messages[1].Content, strings.Repeat("x", 4001)) {
			t.Error("unbounded or private input")
		}
		want := "haiku"
		review := true
		if count == 2 {
			want = "terra"
			review = false
		}
		if req.Model != want {
			t.Errorf("model %s", req.Model)
		}
		content := fmt.Sprintf(`{"match":1,"sameWork":true,"sameEdition":false,"needsReview":%t,"reason":"evidence","fieldSources":{"description":1}}`, review)
		_, _ = fmt.Fprintf(w, `{"model":%q,"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":7}}`, req.Model, content)
	}))
	defer s.Close()
	c := New(s.URL, "test-key", "haiku", "terra")
	d, err := c.Judge(context.Background(), domain.Book{Files: []domain.File{{AbsolutePath: "/private/book.epub"}}}, []domain.Candidate{{Metadata: domain.Metadata{Description: strings.Repeat("x", 5000)}}})
	if err != nil || count != 2 || d.Model != "terra" || d.InputTokens != 24 || d.OutputTokens != 14 {
		t.Fatalf("decision %+v count %d error %v", d, count, err)
	}
}

func TestJudgeErrorsDoNotFallbackOrLeak(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{{"http", "test-key", 500}, {"malformed", `{"choices":[{"message":{"content":"{}"}}]}`, 200}} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count++
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			_, err := New(s.URL, "test-key", "a", "b").Judge(context.Background(), domain.Book{}, nil)
			if err == nil || strings.Contains(err.Error(), "test-key") || count != 1 {
				t.Fatalf("err=%v requests=%d", err, count)
			}
		})
	}
}
