package domain

import (
	"encoding/json"
	"strings"
)

// Coverage counts populated bibliographic fields, not metadata quality. Optional
// subtitle/series and provider links do not inflate the score.
type Coverage struct {
	Before  int      `json:"before"`
	After   int      `json:"after"`
	Total   int      `json:"total"`
	Missing []string `json:"missing,omitempty"`
}

func MetadataCoverage(metadata Metadata, patch Patch) Coverage {
	raw, _ := json.Marshal(metadata)
	before := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &before)
	after := map[string]json.RawMessage{}
	for field, value := range before {
		after[field] = value
	}
	for field, value := range patch {
		after[field] = value
	}
	groups := []struct {
		name   string
		fields []string
	}{
		{"Title", []string{"title"}},
		{"Authors", []string{"authors"}},
		{"Description", []string{"description"}},
		{"Language", []string{"language"}},
		{"Publisher", []string{"publisher"}},
		{"Publication date", []string{"publishedDate", "publishedYear"}},
		{"Page count", []string{"pageCount"}},
		{"ISBN", []string{"isbn10", "isbn13"}},
		{"Genres", []string{"genres"}},
	}
	c := Coverage{Total: len(groups)}
	for _, group := range groups {
		b, a := false, false
		for _, field := range group.fields {
			b = b || Populated(before[field])
			a = a || Populated(after[field])
		}
		if b {
			c.Before++
		}
		if a {
			c.After++
		} else {
			c.Missing = append(c.Missing, group.name)
		}
	}
	return c
}

func Populated(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	s := strings.TrimSpace(string(raw))
	if s == "null" || s == "0" || s == "[]" || s == "{}" || s == "false" {
		return false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		for _, item := range list {
			if strings.TrimSpace(item) != "" {
				return true
			}
		}
		return false
	}
	return true
}
