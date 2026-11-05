package domain

import "testing"

func TestLanguageIdentity(t *testing.T) {
	for _, s := range []string{"English", "en", "eng", "en-US", "en-GB"} {
		if LanguageKey(s) != "en" {
			t.Errorf("%s != en", s)
		}
	}
	if LanguageKey("Hindi") != "hi" || LanguageKey("Spanish; Castilian") != "es" || LanguageKey("") != "" || LanguageKey("unknown one") == LanguageKey("unknown two") {
		t.Fatal("unsafe language normalization")
	}
}
