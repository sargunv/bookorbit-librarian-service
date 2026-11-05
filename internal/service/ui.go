package service

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sargunv/bookorbit-librarian-service/internal/curator"
	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

//go:embed web/*
var webFiles embed.FS

var activityTemplate = template.Must(template.New("activity.html").Funcs(template.FuncMap{
	"cursor": func(id int64) int64 { return id + 1 },
	"when": func(raw string) string {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return raw
		}
		return t.UTC().Format("Jan 2, 15:04 UTC")
	},
	"value": func(raw json.RawMessage) string {
		if len(raw) == 0 || string(raw) == "null" {
			return "Not set"
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		var values []string
		if json.Unmarshal(raw, &values) == nil {
			return strings.Join(values, ", ")
		}
		return string(raw)
	},
	"field": func(raw string) string {
		names := map[string]string{"isbn10": "ISBN-10", "isbn13": "ISBN-13", "hardcoverId": "Hardcover link", "hardcoverEditionId": "Hardcover edition", "goodreadsId": "Goodreads link", "googleBooksId": "Google Books link", "openLibraryId": "Open Library link", "publishedYear": "Publication year", "publishedDate": "Publication date", "pageCount": "Page count", "seriesName": "Series", "seriesIndex": "Series position"}
		if label := names[raw]; label != "" {
			return label
		}
		if raw == "" {
			return raw
		}
		return strings.ToUpper(raw[:1]) + raw[1:]
	},
	"outcome": eventOutcome,
	"bookURL": func(base string, id int) string {
		if base == "" {
			return ""
		}
		return fmt.Sprintf("%s/book/%d", strings.TrimRight(base, "/"), id)
	},
}).ParseFS(webFiles, "web/activity.html"))

func eventOutcome(e domain.ActivityEvent) string {
	switch e.Kind {
	case "applied":
		if len(e.Details.Changes) == 0 && !e.Details.CoverChanged {
			if e.Details.MovedFiles > 0 || len(e.Details.MergedBooks) > 0 {
				return "Files organized"
			}
			return "Metadata kept"
		}
		return "Updated"
	case "retry":
		return "Retry scheduled"
	case "failed":
		return "Needs attention"
	case "skipped":
		return "Skipped"
	case "planned":
		return "Plan saved"
	}
	return e.Kind
}

type activityPage struct {
	Counts                    map[string]int
	Total, Completed, Percent int
	Paused                    bool
	Active                    []domain.ActivityEvent
	Events                    []domain.ActivityEvent
	Summaries                 []domain.ActivitySummary
	SummaryError, LibraryURL  string
	Next                      int64
	History                   bool
}

func (s *Service) mountUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /assets/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name != "activity.css" && name != "activity.js" {
			http.NotFound(w, r)
			return
		}
		data, err := webFiles.ReadFile("web/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if name == "activity.css" {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("GET /{$}", s.activityPage)
}

func (s *Service) activityPage(w http.ResponseWriter, r *http.Request) {
	// This is a separate, read-only capability. It never authenticates /v1/.
	if s.cfg.UIProxyToken == "" {
		http.NotFound(w, r)
		return
	}
	proxy := r.Header.Get("X-Librarian-UI-Token")
	admin := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(proxy), []byte(s.cfg.UIProxyToken)) != 1 && (s.cfg.AdminToken == "" || subtle.ConstantTimeCompare([]byte(admin), []byte(s.cfg.AdminToken)) != 1) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	counts, err := s.db.PolicyCounts(r.Context(), curator.PolicyVersion)
	if err != nil {
		http.Error(w, "activity unavailable", http.StatusServiceUnavailable)
		return
	}
	active, err := s.db.ActiveActivity(r.Context(), curator.PolicyVersion)
	if err != nil {
		http.Error(w, "activity unavailable", http.StatusServiceUnavailable)
		return
	}
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if r.URL.Query().Get("before") == "" {
		before = 0
		err = nil
	}
	if err != nil || before < 0 {
		http.Error(w, "invalid activity cursor", 400)
		return
	}
	history := r.URL.Query().Get("history") == "all"
	policy := curator.PolicyVersion
	if history {
		policy = ""
	}
	events, err := s.db.Activity(r.Context(), policy, before, 30)
	if err != nil {
		http.Error(w, "activity unavailable", http.StatusServiceUnavailable)
		return
	}
	summaries, err := s.db.Summaries(r.Context(), curator.PolicyVersion, 3)
	if err != nil {
		http.Error(w, "summaries unavailable", http.StatusServiceUnavailable)
		return
	}
	page := activityPage{Counts: counts, Paused: s.paused.Load(), Active: active, Events: events, Summaries: summaries, LibraryURL: s.cfg.BookOrbitPublicURL, History: history}
	for _, count := range counts {
		page.Total += count
	}
	page.Completed = counts["applied"] + counts["skipped"] + counts["failed"]
	if page.Total > 0 {
		page.Percent = 100 * page.Completed / page.Total
	}
	if len(events) == 30 {
		page.Next = events[len(events)-1].ID
	}
	s.summaryMu.RLock()
	page.SummaryError = s.summaryError
	s.summaryMu.RUnlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err = activityTemplate.Execute(w, page); err != nil {
		return
	}
}
