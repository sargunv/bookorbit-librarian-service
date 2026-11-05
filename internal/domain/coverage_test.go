package domain

import (
	"encoding/json"
	"testing"
)

func TestCoverageCountsPresenceNotOptionalFieldsOrProviderLinks(t *testing.T) {
	before := Metadata{Title: "A Book", Authors: []string{"An Author"}, PublishedYear: 2000, ISBN13: "9780000000000", Subtitle: "Optional", SeriesName: "Optional"}
	patch := Patch{"publishedDate": json.RawMessage(`"2000-01-01"`), "isbn10": json.RawMessage(`"0000000000"`), "hardcoverId": json.RawMessage(`"123"`), "description": json.RawMessage(`"A description"`)}
	c := MetadataCoverage(before, patch)
	if c.Total != 9 || c.Before != 4 || c.After != 5 {
		t.Fatalf("coverage: %+v", c)
	}
	if Populated(json.RawMessage(`"   "`)) || Populated(json.RawMessage(`[" "]`)) {
		t.Fatal("whitespace counted as metadata")
	}
}
