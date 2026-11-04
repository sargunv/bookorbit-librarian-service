package service

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sargunv/bookorbit-librarian-service/internal/config"
	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func TestOperationsRequireAuthentication(t *testing.T) {
	d := t.TempDir()
	cfg := config.Config{StatePath: filepath.Join(d, "state.db"), LibraryRoot: filepath.Join(d, "library"), OriginalsRoot: filepath.Join(d, "originals"), AdminToken: strings.Repeat("x", 32)}
	s, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.Close() }()
	for _, path := range []string{"/v1/status", "/v1/jobs"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		r = httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+cfg.AdminToken)
		w = httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("authenticated %s: %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), cfg.AdminToken) {
			t.Fatal("credential exposed")
		}
	}
	r := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestAppliedProjectionAllowsCrashResumeButDetectsOtherChanges(t *testing.T) {
	b := domain.Book{Metadata: domain.Metadata{Title: "Dune"}, ProviderIDs: map[string]string{"hardcover": "42"}}
	p := domain.Patch{"title": json.RawMessage(`"Dune"`), "hardcoverId": json.RawMessage(`"42"`)}
	if !patchMatches(b, p) {
		t.Fatal("applied projection not recognized")
	}
	b.Metadata.Title = "A different book"
	if patchMatches(b, p) {
		t.Fatal("conflicting edit accepted")
	}
}
