package service

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

type (
	changeBadge struct{ Field, Action, Class string }
	activityRow struct {
		domain.ActivityEvent
		Impact, Label, Explanation string
		Badges                     []changeBadge
		Providers                  []string
		Delta                      int
		Missing                    string
		BeforeCover, AfterCover    bool
		Recovered                  bool
	}
)

type (
	activityStats  struct{ Major, Metadata, Covers, Links, Unchanged, Filled, Issues int }
	activityFilter struct {
		Key, Label string
		Count      int
		Selected   bool
	}
)

func activityView(e domain.ActivityEvent) activityRow {
	r := activityRow{ActivityEvent: e}
	metadata, links := 0, 0
	for _, change := range e.Details.Changes {
		if isLink(change.Field) {
			links++
			continue
		}
		metadata++
		action, class := "Updated", "corrected"
		if !domain.Populated(change.Before) {
			action, class = "Added", "added"
		}
		if change.Field == "description" && action != "Added" {
			action = "Rewritten"
		}
		r.Badges = append(r.Badges, changeBadge{Field: fieldName(change.Field), Action: action, Class: class})
	}
	if links > 0 {
		r.Badges = append(r.Badges, changeBadge{Field: fmt.Sprintf("%d provider link%s", links, plural(links)), Action: "Updated", Class: "link"})
	}
	switch {
	case e.Kind == "retry":
		r.Impact, r.Label = "issues", "Retry scheduled"
	case e.Kind == "failed":
		r.Impact, r.Label = "issues", "Failed"
	case e.Kind == "skipped":
		r.Impact, r.Label = "skipped", "Skipped"
	case e.Kind == "planned":
		r.Impact, r.Label = "planned", "Planned"
	case metadata >= 3 || (metadata > 0 && e.Details.CoverChanged):
		r.Impact, r.Label = "major", "Major update"
	case metadata > 0:
		r.Impact, r.Label = "metadata", "Metadata update"
	case e.Details.CoverChanged:
		r.Impact, r.Label = "cover", "Cover update"
	case links > 0:
		r.Impact, r.Label = "links", "Links only"
	case len(e.Details.MergedBooks) > 0:
		r.Impact, r.Label = "files", "Formats grouped"
	case e.Details.MovedFiles > 0:
		r.Impact, r.Label = "files", "Files only"
	default:
		r.Impact, r.Label = "unchanged", "Unchanged"
	}
	if c := e.Details.Coverage; c != nil {
		r.Delta = c.After - c.Before
		r.Missing = strings.Join(c.Missing, ", ")
	}
	sources := map[string]bool{}
	for _, source := range e.Details.Sources {
		sources[providerName(source)] = true
	}
	for _, c := range e.Details.Changes {
		if c.Source != "" {
			sources[providerName(c.Source)] = true
		}
	}
	if e.Details.CoverSource != "" {
		sources[providerName(e.Details.CoverSource)] = true
	}
	for p := range sources {
		r.Providers = append(r.Providers, p)
	}
	sort.Strings(r.Providers)
	return r
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func isLink(field string) bool {
	switch field {
	case "hardcoverId", "hardcoverEditionId", "goodreadsId", "googleBooksId", "openLibraryId":
		return true
	}
	return false
}

func providerName(source string) string {
	provider, _, _ := strings.Cut(source, "/")
	switch strings.ToLower(provider) {
	case "hardcover":
		return "Hardcover"
	case "goodreads":
		return "Goodreads"
	case "google", "googlebooks":
		return "Google Books"
	case "openlibrary":
		return "Open Library"
	}
	return provider
}

func fieldName(raw string) string {
	names := map[string]string{"isbn10": "ISBN-10", "isbn13": "ISBN-13", "hardcoverId": "Hardcover link", "hardcoverEditionId": "Hardcover edition", "goodreadsId": "Goodreads link", "googleBooksId": "Google Books link", "openLibraryId": "Open Library link", "publishedYear": "Year", "publishedDate": "Publication date", "pageCount": "Pages", "seriesName": "Series", "seriesIndex": "Series position"}
	if label := names[raw]; label != "" {
		return label
	}
	if raw == "" {
		return raw
	}
	return strings.ToUpper(raw[:1]) + raw[1:]
}

func matchesFilter(r activityRow, filter string) bool {
	switch filter {
	case "major":
		return r.Impact == "major"
	case "covers":
		return r.Details.CoverChanged
	case "descriptions":
		for _, c := range r.Details.Changes {
			if c.Field == "description" {
				return true
			}
		}
		return false
	case "links":
		return r.Impact == "links"
	case "unchanged":
		return r.Impact == "unchanged" || r.Impact == "files"
	case "issues":
		return r.Impact == "issues" && !r.Recovered
	}
	return true
}
