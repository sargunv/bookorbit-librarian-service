package domain

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"golang.org/x/text/language"
)

// ISBNKey validates an ISBN and compares its edition identity. ISBN-10 -> 13
// conversion is used only for comparison, never to invent a catalog identifier.
func ISBNKey(value string) string {
	s := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(value))
	if len(s) == 10 {
		sum := 0
		for i, r := range s {
			n := int(r - '0')
			if i == 9 && r == 'X' {
				n = 10
			}
			if n < 0 || n > 9 && (i != 9 || n != 10) {
				return ""
			}
			sum += (10 - i) * n
		}
		if sum%11 != 0 {
			return ""
		}
		s = "978" + s[:9]
		sum = 0
		for i, r := range s {
			sum += int(r-'0') * (1 + 2*(i%2))
		}
		return s + string(rune('0'+(10-sum%10)%10))
	}
	if len(s) == 13 && (strings.HasPrefix(s, "978") || strings.HasPrefix(s, "979")) {
		sum := 0
		for i, r := range s {
			if r < '0' || r > '9' {
				return ""
			}
			sum += int(r-'0') * (1 + 2*(i%2))
		}
		if sum%10 == 0 {
			return s
		}
	}
	return ""
}

// RejectTranslatedISBNs removes an identifier positively tied to a rejected
// translation. An absent ISBN in a source is not evidence for deletion.
func RejectTranslatedISBNs(book Book, patch Patch, evidence []Proposal) map[string]string {
	cleared := map[string]string{}
	lang := LanguageKey(book.Metadata.Language)
	if raw := patch["language"]; len(raw) > 0 {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			lang = LanguageKey(s)
		}
	}
	if tag, err := language.Parse(lang); err != nil || tag == language.Und {
		return cleared
	}
	values := Values(book)
	for field, v := range patch {
		values[field] = v
	}
	for _, field := range []string{"isbn10", "isbn13"} {
		if slices.Contains(book.LockedFields, field) {
			continue
		}
		var value string
		if json.Unmarshal(values[field], &value) != nil || ISBNKey(value) == "" {
			continue
		}
		supported := false
		rejected := ""
		for _, p := range evidence {
			selected := append([]int{p.Decision.Match}, p.Decision.WorkSources...)
			selected = append(selected, p.Decision.EditionSources...)
			for i, c := range p.Candidates {
				if ISBNKey(c.ISBN10) != ISBNKey(value) && ISBNKey(c.ISBN13) != ISBNKey(value) {
					continue
				}
				if slices.Contains(selected, i+1) {
					supported = true
					continue
				}
				candidateLang := LanguageKey(c.Language)
				if tag, err := language.Parse(candidateLang); err == nil && tag != language.Und && candidateLang != lang {
					rejected = "rejected translated edition: " + c.Provider + "/" + c.ProviderID
				}
			}
		}
		if !supported && rejected != "" {
			patch[field] = json.RawMessage("null")
			cleared[field] = rejected
		}
	}
	return cleared
}

// CoherentISBNPatch clears a stale, conflicting counterpart after an explicit
// edition selection. Missing source data never authorizes retaining a different
// edition's ISBN. A locked conflict or inconsistent new source pair is an error.
func CoherentISBNPatch(book Book, patch Patch) error {
	values := map[string]string{"isbn10": book.Metadata.ISBN10, "isbn13": book.Metadata.ISBN13}
	for _, field := range []string{"isbn10", "isbn13"} {
		if raw, ok := patch[field]; ok {
			var value string
			if string(raw) != "null" {
				if err := json.Unmarshal(raw, &value); err != nil {
					return err
				}
			}
			if value != "" && ISBNKey(value) == "" {
				return errors.New("selected ISBN has an invalid checksum")
			}
			values[field] = value
		}
	}
	if values["isbn10"] == "" || values["isbn13"] == "" || ISBNKey(values["isbn10"]) != "" && ISBNKey(values["isbn10"]) == ISBNKey(values["isbn13"]) {
		return nil
	}
	_, ten := patch["isbn10"]
	_, thirteen := patch["isbn13"]
	if !ten && !thirteen {
		return nil
	}
	if ten && thirteen {
		return errors.New("selected ISBN pair identifies different editions")
	}
	clear := "isbn13"
	if thirteen {
		clear = "isbn10"
	}
	if slices.Contains(book.LockedFields, clear) {
		return errors.New("retained ISBN conflicts with a locked counterpart")
	}
	patch[clear] = json.RawMessage("null")
	return nil
}
