// Package reconcile reuses persisted curation; it never calls a model or provider.
package reconcile

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
	"github.com/sargunv/bookorbit-librarian-service/internal/store"
)

type Plan struct {
	Version int         `json:"version"`
	Groups  []Group     `json:"groups"`
	Aliases map[int]int `json:"aliases"`
}

type Group struct {
	Survivor     int               `json:"survivor"`
	BookIDs      []int             `json:"bookIds"`
	FileIDs      []int             `json:"fileIds"`
	JobID        int64             `json:"jobId"`
	Before       domain.Patch      `json:"before"`
	Patch        domain.Patch      `json:"patch"`
	Sources      map[string]string `json:"sources"`
	CoverURL     string            `json:"coverUrl,omitempty"`
	CoverSource  string            `json:"coverSource,omitempty"`
	ReviewReason string            `json:"reviewReason,omitempty"`
}

// HistoricalAliases retains every old merge, including earlier curation policies.
func HistoricalAliases(jobs []store.Job) (map[int]int, error) {
	aliases := map[int]int{}
	for _, j := range jobs {
		var r struct {
			Merged []int `json:"mergedBookIds"`
		}
		if len(j.Result) == 0 {
			continue
		}
		if err := json.Unmarshal(j.Result, &r); err != nil {
			return nil, err
		}
		for _, id := range r.Merged {
			if prior, ok := aliases[id]; ok && prior != j.BookID {
				return nil, fmt.Errorf("conflicting merge history for book %d", id)
			}
			aliases[id] = j.BookID
		}
	}
	for id := range aliases {
		seen := map[int]bool{id: true}
		next := aliases[id]
		for aliases[next] != 0 {
			if seen[next] {
				return nil, fmt.Errorf("cyclic merge history for book %d", id)
			}
			seen[next] = true
			next = aliases[next]
		}
		aliases[id] = next
	}
	return aliases, nil
}

func workIDs(b domain.Book, p *domain.Proposal) map[string]bool {
	ids := map[string]bool{}
	if p == nil || p.Decision.Match == 0 {
		return ids
	}
	for _, n := range append([]int{p.Decision.Match}, p.Decision.WorkSources...) {
		if n > 0 && n <= len(p.Candidates) {
			c := p.Candidates[n-1]
			if c.ProviderID != "" {
				ids[strings.ToLower(c.Provider)+":"+c.ProviderID] = true
			}
		}
	}
	return ids
}

func sameWork(a, b domain.Book, pa, pb *domain.Proposal) bool {
	if a.LibraryFolderID != b.LibraryFolderID {
		return false
	}
	if a.LibraryID != b.LibraryID || domain.Normalize(a.Metadata.Title) == "" || domain.Normalize(a.Metadata.Title) != domain.Normalize(b.Metadata.Title) || domain.LanguageKey(a.Metadata.Language) == "" || domain.LanguageKey(a.Metadata.Language) != domain.LanguageKey(b.Metadata.Language) {
		return false
	}
	authors := func(names []string) string {
		out := make([]string, 0, len(names))
		for _, n := range names {
			if n = domain.Normalize(n); n != "" {
				out = append(out, n)
			}
		}
		slices.Sort(out)
		return strings.Join(out, "\x00")
	}
	if authors(a.Metadata.Authors) == "" || authors(a.Metadata.Authors) != authors(b.Metadata.Authors) {
		return false
	}
	// Identifier equality alone never bypasses title/author/language checks.
	if a.HardcoverEditionID != "" && a.HardcoverEditionID == b.HardcoverEditionID {
		return true
	}
	for _, x := range []string{a.Metadata.ISBN10, a.Metadata.ISBN13} {
		for _, y := range []string{b.Metadata.ISBN10, b.Metadata.ISBN13} {
			if domain.ISBNKey(x) != "" && domain.ISBNKey(x) == domain.ISBNKey(y) {
				return true
			}
		}
	}
	left, right := workIDs(a, pa), workIDs(b, pb)
	for id := range left {
		if right[id] {
			return true
		}
	}
	return false
}

// Build chooses a stable survivor, requires pairwise identity, and prefers
// explicit saved field selections over a sibling's unchanged old metadata.
// Edition fields remain the survivor's unless missing; files keep their IDs.
func Build(books []domain.Book, jobs []store.Job, known map[int]int) (Plan, error) {
	p := Plan{Version: 1, Aliases: map[int]int{}}
	aliases, err := HistoricalAliases(jobs)
	if err != nil {
		return p, err
	}
	for old, dest := range known {
		aliases[old] = dest
	}
	for old := range aliases {
		seen := map[int]bool{old: true}
		dest := aliases[old]
		for aliases[dest] != 0 {
			if seen[dest] {
				return p, fmt.Errorf("cyclic book alias %d", old)
			}
			seen[dest] = true
			dest = aliases[dest]
		}
		aliases[old] = dest
	}
	saved := map[int]store.Job{}
	for _, j := range jobs {
		if j.Proposal != nil && j.Proposal.Decision.Match != 0 && j.ID > saved[j.BookID].ID {
			saved[j.BookID] = j
		}
	}
	slices.SortFunc(books, func(a, b domain.Book) int { return a.ID - b.ID })
	byID := map[int]domain.Book{}
	for _, b := range books {
		byID[b.ID] = b
	}
	for old, dest := range aliases {
		if _, ok := byID[dest]; !ok {
			return p, fmt.Errorf("historical merge target %d is missing", dest)
		}
		p.Aliases[old] = dest
	}
	used := map[int]bool{}
	for _, base := range books {
		if used[base.ID] {
			continue
		}
		members := []domain.Book{base}
		used[base.ID] = true
		for _, other := range books {
			if used[other.ID] {
				continue
			}
			good := true
			for _, m := range members {
				if !sameWork(m, other, saved[m.ID].Proposal, saved[other.ID].Proposal) {
					good = false
					break
				}
			}
			if good {
				members = append(members, other)
				used[other.ID] = true
			}
		}
		g := Group{Survivor: base.ID, JobID: saved[base.ID].ID, Before: domain.Values(base), Patch: domain.Patch{}, Sources: map[string]string{}}
		var evidence []store.Job
		for _, m := range members {
			g.BookIDs = append(g.BookIDs, m.ID)
			for _, f := range m.Files {
				g.FileIDs = append(g.FileIDs, f.ID)
			}
			if j, ok := saved[m.ID]; ok {
				evidence = append(evidence, j)
			}
			for old, dest := range aliases {
				if dest == m.ID {
					p.Aliases[old] = base.ID
					if j, ok := saved[old]; ok {
						evidence = append(evidence, j)
					}
				}
			}
			if m.ID != base.ID {
				p.Aliases[m.ID] = base.ID
			}
		}
		if len(members) > 1 || len(evidence) > 1 {
			selectFields(&g, evidence, base, aliases)
		}
		var cover store.Job
		memberIDs := map[int]bool{}
		for _, m := range members {
			memberIDs[m.ID] = true
		}
		for _, j := range jobs {
			id := j.BookID
			if dest := aliases[id]; dest != 0 {
				id = dest
			}
			if memberIDs[id] && j.Proposal != nil && j.Proposal.CoverURL != "" && j.ID > cover.ID {
				cover = j
			}
		}
		if cover.Proposal != nil && cover.BookID != base.ID && !slices.Contains(base.LockedFields, "cover") {
			g.CoverURL, g.CoverSource = cover.Proposal.CoverURL, cover.Proposal.Provenance["cover"]
		}
		// Reuse explicit ISBN selections even when an old version left their
		// contradictory counterpart untouched. No new provider lookup is needed.
		if domain.ISBNKey(base.Metadata.ISBN10) != domain.ISBNKey(base.Metadata.ISBN13) && base.Metadata.ISBN10 != "" && base.Metadata.ISBN13 != "" {
			if j := saved[base.ID]; j.Proposal != nil {
				for _, field := range []string{"isbn10", "isbn13"} {
					if j.Proposal.Decision.FieldSources[field] > 0 {
						value := j.Proposal.Patch[field]
						if len(value) == 0 {
							value = domain.Values(j.Proposal.Book)[field]
						}
						if len(value) > 0 {
							g.Patch[field] = value
							g.Sources[field] = j.Proposal.Provenance[field]
						}
					}
				}
			}
		}
		var proposals []domain.Proposal
		for _, j := range evidence {
			if j.Proposal != nil {
				proposals = append(proposals, *j.Proposal)
			}
		}
		for field, source := range domain.RejectTranslatedISBNs(base, g.Patch, proposals) {
			g.Sources[field] = source
		}
		if err = domain.CoherentISBNPatch(base, g.Patch); err != nil {
			return p, fmt.Errorf("book %d: %w", base.ID, err)
		}
		for _, field := range []string{"isbn10", "isbn13"} {
			if string(g.Patch[field]) == "null" && g.Sources[field] == "" {
				g.Sources[field] = "cleared conflicting retained ISBN counterpart"
			}
		}
		if lang := domain.LanguageKey(base.Metadata.Language); lang != "" && lang != base.Metadata.Language {
			g.Patch["language"], _ = json.Marshal(lang)
			g.Sources["language"] = "language tag normalization"
		}
		for field := range g.Patch {
			if slices.Contains(base.LockedFields, field) || domain.EqualField(field, g.Patch[field], g.Before[field]) {
				delete(g.Patch, field)
				delete(g.Sources, field)
			}
		}
		// A locked value on any sibling must survive, never disappear silently.
		for _, m := range members[1:] {
			for _, field := range m.LockedFields {
				want := g.Patch[field]
				if len(want) == 0 {
					want = g.Before[field]
				}
				if v := domain.Values(m)[field]; len(v) > 0 && !domain.EqualField(field, v, want) {
					return p, fmt.Errorf("book %d has a conflicting locked field %s", m.ID, field)
				}
			}
		}
		if len(g.Patch) > 0 || len(members) > 1 || g.CoverURL != "" {
			p.Groups = append(p.Groups, g)
		}
	}
	return p, nil
}

func selectFields(g *Group, jobs []store.Job, base domain.Book, aliases map[int]int) {
	// Most recent explicit selection wins a tie. All input proposals remain in
	// SQLite and the private plan records the chosen field's exact source.
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	workFields := map[string]bool{"title": true, "subtitle": true, "description": true, "authors": true, "genres": true, "language": true, "seriesName": true, "seriesIndex": true}
	for _, j := range jobs {
		p := j.Proposal
		for field, index := range p.Decision.FieldSources {
			if !workFields[field] || index <= 0 || index > len(p.Candidates) {
				continue
			}
			value := p.Patch[field]
			if len(value) == 0 {
				raw, _ := json.Marshal(p.Candidates[index-1].Metadata)
				var candidate domain.Patch
				_ = json.Unmarshal(raw, &candidate)
				if !domain.Populated(candidate[field]) {
					continue
				}
				value = domain.Values(p.Book)[field]
			}
			if len(value) > 0 && string(value) != "null" && string(value) != `""` {
				g.Patch[field] = value
				g.Sources[field] = p.Candidates[index-1].Provider + "/" + p.Candidates[index-1].ProviderID
			}
		}
		for field, value := range p.Patch {
			if !workFields[field] && len(g.Before[field]) == 0 && len(g.Patch[field]) == 0 {
				if field != "hardcoverId" && field != "goodreadsId" && field != "googleBooksId" && field != "openLibraryId" && j.BookID != base.ID {
					// Keep one coherent edition exemplar. A different live format
					// cannot fill isolated edition fields on the survivor.
					if aliases[j.BookID] == 0 || !sameEdition(base, *p) {
						continue
					}
				}
				g.Patch[field], g.Sources[field] = value, p.Provenance[field]
			}
		}
	}
}

func sameEdition(base domain.Book, p domain.Proposal) bool {
	values := domain.Values(p.Book)
	for k, v := range p.Patch {
		values[k] = v
	}
	for _, field := range []string{"isbn10", "isbn13"} {
		var v string
		_ = json.Unmarshal(values[field], &v)
		if key := domain.ISBNKey(v); key != "" && (key == domain.ISBNKey(base.Metadata.ISBN10) || key == domain.ISBNKey(base.Metadata.ISBN13)) {
			return true
		}
	}
	return base.HardcoverEditionID != "" && base.HardcoverEditionID == p.Book.HardcoverEditionID
}
