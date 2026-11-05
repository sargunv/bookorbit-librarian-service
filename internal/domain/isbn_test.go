package domain

import (
	"encoding/json"
	"testing"
)

func TestISBNIdentityAndConflictingRetainedCounterpart(t *testing.T) {
	if ISBNKey("0-306-40615-2") != "9780306406157" || ISBNKey("9780306406157") != "9780306406157" || ISBNKey("9780306406150") != "" {
		t.Fatal("ISBN validation/comparison failed")
	}
	book := Book{Metadata: Metadata{ISBN13: "9780131103627"}}
	patch := Patch{"isbn10": json.RawMessage(`"0306406152"`)}
	if err := CoherentISBNPatch(book, patch); err != nil {
		t.Fatal(err)
	}
	if string(patch["isbn13"]) != "null" {
		t.Fatal("retained a different edition's ISBN13")
	}
	book.LockedFields = []string{"isbn13"}
	if err := CoherentISBNPatch(book, Patch{"isbn10": json.RawMessage(`"0306406152"`)}); err == nil {
		t.Fatal("cleared locked ISBN")
	}
	if err := CoherentISBNPatch(Book{}, Patch{"isbn10": json.RawMessage(`"0306406152"`), "isbn13": json.RawMessage(`"9780131103627"`)}); err == nil {
		t.Fatal("accepted conflicting selected identifiers")
	}
	if !EqualField("isbn13", json.RawMessage("null"), nil) {
		t.Fatal("cleared field did not verify against omitted API field")
	}
}

func TestRejectedTranslationISBNRequiresPositiveEvidence(t *testing.T) {
	b := Book{Metadata: Metadata{Language: "English", ISBN13: "9780131103627"}}
	e := Proposal{Candidates: []Candidate{{Provider: "source", ProviderID: "english", Metadata: Metadata{Language: "en"}}, {Provider: "source", ProviderID: "translation", Metadata: Metadata{Language: "French", ISBN13: "9780131103627"}}}, Decision: Decision{Match: 1, WorkSources: []int{1}}}
	p := Patch{}
	if cleared := RejectTranslatedISBNs(b, p, []Proposal{e}); len(cleared) != 1 || string(p["isbn13"]) != "null" {
		t.Fatal("retained positively contradicted translation identifier")
	}
	e.Candidates = e.Candidates[:1]
	p = Patch{}
	if len(RejectTranslatedISBNs(b, p, []Proposal{e})) != 0 || len(p) != 0 {
		t.Fatal("missing candidate ISBN was treated as evidence for removal")
	}
}
