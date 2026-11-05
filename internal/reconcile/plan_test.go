package reconcile

import (
	"encoding/json"
	"testing"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
	"github.com/sargunv/bookorbit-librarian-service/internal/store"
)

func example(id int, lang string) domain.Book {
	return domain.Book{ID: id, LibraryID: 1, Metadata: domain.Metadata{Title: "Example Novel", Authors: []string{"A. Writer"}, Language: lang}, Files: []domain.File{{ID: id, Format: "epub"}}}
}

func saved(b domain.Book, fields map[string]int, patch domain.Patch) store.Job {
	return store.Job{ID: int64(b.ID), BookID: b.ID, Proposal: &domain.Proposal{Book: b, Candidates: []domain.Candidate{{Provider: "hardcover", ProviderID: "example-novel"}}, Decision: domain.Decision{Match: 1, WorkSources: []int{1}, FieldSources: fields}, Patch: patch}}
}

func TestAlternateFormatsReuseWorkEvidenceWithoutInventingISBN(t *testing.T) {
	a, b := example(1, "English"), example(2, "en-US")
	b.Files[0].Format = "pdf"
	p, err := Build([]domain.Book{b, a}, []store.Job{saved(a, nil, nil), saved(b, nil, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Groups) != 1 || len(p.Groups[0].BookIDs) != 2 || p.Groups[0].Survivor != 1 || p.Aliases[2] != 1 {
		t.Fatalf("bad plan: %+v", p)
	}
	if string(p.Groups[0].Patch["language"]) != `"en"` || len(p.Groups[0].Patch["isbn13"]) != 0 {
		t.Fatal("language was not normalized or ISBN was fabricated")
	}
}

func TestDeletedRecordsExplicitSeriesSurvivesLaterPreserveDecision(t *testing.T) {
	old, current := example(1, "en"), example(2, "en")
	old.Metadata.SeriesName = "Old Universe"
	current.Metadata.SeriesName = "Old Publishing Project"
	a := saved(old, map[string]int{"seriesName": 1, "seriesIndex": 1}, domain.Patch{"seriesName": json.RawMessage(`"Actual Series"`), "seriesIndex": json.RawMessage(`"1"`)})
	b := saved(current, nil, nil)
	b.Result = json.RawMessage(`{"mergedBookIds":[1]}`)
	p, err := Build([]domain.Book{current}, []store.Job{a, b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Groups) != 1 || string(p.Groups[0].Patch["seriesName"]) != `"Actual Series"` || p.Aliases[1] != 2 {
		t.Fatalf("lost saved selection: %+v", p)
	}
}

func TestIdentityRejectsTranslationsDifferentAuthorsAndUnprovenTitles(t *testing.T) {
	for _, kind := range []string{"translation", "author", "unproven"} {
		t.Run(kind, func(t *testing.T) {
			a, b := example(1, "en"), example(2, "en")
			jobs := []store.Job{saved(a, nil, nil), saved(b, nil, nil)}
			switch kind {
			case "translation":
				b.Metadata.Language = "fr"
			case "author":
				b.Metadata.Authors = []string{"Other Writer"}
			case "unproven":
				jobs = nil
			}
			p, err := Build([]domain.Book{a, b}, jobs, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Aliases) != 0 {
				t.Fatalf("unsafe match: %+v", p)
			}
		})
	}
}

func TestIdentityRequiresPairwiseEvidenceAndHonorsLocks(t *testing.T) {
	a, b, c := example(1, "en"), example(2, "en"), example(3, "en")
	a.Metadata.ISBN13 = "9780306406157"
	b.Metadata.ISBN13 = "9780306406157"
	b.Metadata.ISBN10 = "0131103628"
	c.Metadata.ISBN10 = "0131103628"
	p, err := Build([]domain.Book{a, b, c}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Groups) != 1 || len(p.Groups[0].BookIDs) != 2 {
		t.Fatalf("transitive bridge matched: %+v", p)
	}
	b.LockedFields = []string{"seriesName"}
	b.Metadata.SeriesName = "Protected"
	if _, err = Build([]domain.Book{a, b}, nil, nil); err == nil {
		t.Fatal("discarded sibling's locked metadata")
	}
}

func TestHistoricalAliasChainsAndRetry(t *testing.T) {
	b := example(3, "en")
	jobs := []store.Job{{BookID: 2, Result: json.RawMessage(`{"mergedBookIds":[1]}`)}, {BookID: 3, Result: json.RawMessage(`{"mergedBookIds":[2]}`)}}
	p, err := Build([]domain.Book{b}, jobs, nil)
	if err != nil || p.Aliases[1] != 3 || p.Aliases[2] != 3 {
		t.Fatalf("bad history: %+v %v", p, err)
	}
	jobs = append(jobs, store.Job{BookID: 1, Result: json.RawMessage(`{"mergedBookIds":[3]}`)})
	if _, err = Build([]domain.Book{b}, jobs, nil); err == nil {
		t.Fatal("cyclic history accepted")
	}
}
