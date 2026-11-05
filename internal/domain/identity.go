package domain

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/language"
)

func Normalize(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }), " ")
}

// LanguageKey compares language identity, rather than the spelling of a tag.
// Unknown names remain distinct; an empty language never establishes identity.
func LanguageKey(s string) string {
	s = strings.TrimSpace(s)
	aliases := map[string]string{"english": "en", "french": "fr", "german": "de", "spanish": "es", "spanish; castilian": "es", "hindi": "hi", "portuguese": "pt", "italian": "it", "dutch": "nl", "russian": "ru", "japanese": "ja", "chinese": "zh", "arabic": "ar", "polish": "pl"}
	if key := aliases[strings.ToLower(s)]; key != "" {
		return key
	}
	if tag, err := language.Parse(s); err == nil {
		base, _ := tag.Base()
		if base.String() != "und" {
			return base.String()
		}
	}
	return Normalize(s)
}

func Values(b Book) Patch {
	raw, _ := json.Marshal(b.Metadata)
	p := Patch{}
	_ = json.Unmarshal(raw, &p)
	for provider, field := range map[string]string{"hardcover": "hardcoverId", "goodreads": "goodreadsId", "google": "googleBooksId", "openlibrary": "openLibraryId", "openLibrary": "openLibraryId"} {
		if id := b.ProviderIDs[provider]; id != "" {
			p[field], _ = json.Marshal(id)
		}
	}
	if b.HardcoverEditionID != "" {
		p["hardcoverEditionId"], _ = json.Marshal(b.HardcoverEditionID)
	}
	return p
}

// EqualField allows upstream author spelling/spacing normalization, but verifies
// actual values for bibliographic fields and identifiers.
func EqualField(field string, a, b json.RawMessage) bool {
	if len(a) == 0 {
		a = json.RawMessage("null")
	}
	if len(b) == 0 {
		b = json.RawMessage("null")
	}
	if field == "authors" || field == "genres" {
		var x, y []string
		if json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil {
			for i := range x {
				x[i] = strings.Join(strings.Fields(strings.ToLower(x[i])), " ")
			}
			for i := range y {
				y[i] = strings.Join(strings.Fields(strings.ToLower(y[i])), " ")
			}
			slices.Sort(x)
			slices.Sort(y)
			return slices.Equal(x, y)
		}
	}
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && stringJSON(x) == stringJSON(y)
}

func stringJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
