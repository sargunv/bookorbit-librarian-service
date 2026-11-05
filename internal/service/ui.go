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
	"field":  fieldName,
	"signed": func(n int) string { return fmt.Sprintf("%+d", n) },
	"percent": func(n, total int) int {
		if total == 0 {
			return 0
		}
		return 100 * n / total
	},
	"bookURL": func(base string, id int) string {
		if base == "" {
			return ""
		}
		return fmt.Sprintf("%s/book/%d", strings.TrimRight(base, "/"), id)
	},
}).ParseFS(webFiles, "web/activity.html"))

type activityPage struct {
	Counts                           map[string]int
	Total, Completed, Percent        int
	Paused                           bool
	Active                           []domain.ActivityEvent
	Rows                             []activityRow
	Stats                            activityStats
	Filters                          []activityFilter
	SummaryError, LibraryURL, Filter string
	Next                             int64
	History                          bool
}

func (s *Service) authorizeUI(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.UIProxyToken == "" {
		http.NotFound(w, r)
		return false
	}
	proxy := r.Header.Get("X-Librarian-UI-Token")
	admin := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(proxy), []byte(s.cfg.UIProxyToken)) != 1 && (s.cfg.AdminToken == "" || subtle.ConstantTimeCompare([]byte(admin), []byte(s.cfg.AdminToken)) != 1) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
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
	mux.HandleFunc("GET /covers/{job}/{version}", s.thumbnail)
	mux.HandleFunc("GET /{$}", s.activityPage)
}

func (s *Service) activityPage(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeUI(w, r) {
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
	events, err := s.db.Activity(r.Context(), policy, 0, -1)
	if err != nil {
		http.Error(w, "activity unavailable", http.StatusServiceUnavailable)
		return
	}
	summaries, err := s.db.Summaries(r.Context(), policy, 1000)
	if err != nil {
		http.Error(w, "summaries unavailable", http.StatusServiceUnavailable)
		return
	}
	filter := r.URL.Query().Get("filter")
	if filter == "" {
		filter = "all"
	}
	filters := []activityFilter{{Key: "all", Label: "All activity"}, {Key: "major", Label: "Major changes"}, {Key: "covers", Label: "Covers"}, {Key: "descriptions", Label: "Descriptions"}, {Key: "links", Label: "Links only"}, {Key: "unchanged", Label: "No metadata change"}, {Key: "issues", Label: "Errors"}}
	valid := false
	for _, f := range filters {
		valid = valid || f.Key == filter
	}
	if !valid {
		http.Error(w, "invalid activity filter", 400)
		return
	}
	page := activityPage{Counts: counts, Paused: s.paused.Load(), Active: active, LibraryURL: s.cfg.BookOrbitPublicURL, History: history, Filter: filter, Filters: filters}
	explanations := map[int64]string{}
	for _, summary := range summaries {
		for _, highlight := range summary.Highlights {
			for _, id := range highlight.EventIDs {
				if explanations[id] == "" {
					explanations[id] = highlight.Text
				}
			}
		}
	}
	latest := map[int64]domain.ActivityEvent{}
	for _, event := range events {
		if _, ok := latest[event.JobID]; !ok {
			latest[event.JobID] = event
		}
	}
	for _, event := range events {
		row := activityView(event)
		last := latest[event.JobID]
		row.Recovered = event.Kind == "retry" && (last.Kind == "applied" || last.Kind == "skipped")
		if row.Recovered {
			row.Label = "Recovered retry"
		}
		row.Explanation = explanations[event.ID]
		row.BeforeCover = s.hasCover(event.JobID, "before")
		row.AfterCover = s.hasCover(event.JobID, "after")
		for i := range page.Filters {
			if matchesFilter(row, page.Filters[i].Key) {
				page.Filters[i].Count++
			}
			page.Filters[i].Selected = page.Filters[i].Key == filter
		}
		if event.ID == last.ID {
			if event.Kind == "applied" {
				switch row.Impact {
				case "major":
					page.Stats.Major++
					page.Stats.Metadata++
				case "metadata":
					page.Stats.Metadata++
				case "links":
					page.Stats.Links++
				case "files", "unchanged":
					page.Stats.Unchanged++
				}
				if event.Details.CoverChanged {
					page.Stats.Covers++
				}
				page.Stats.Filled += row.Delta
			}
			if row.Impact == "issues" && !row.Recovered {
				page.Stats.Issues++
			}
		}
		if (before == 0 || event.ID < before) && matchesFilter(row, filter) {
			page.Rows = append(page.Rows, row)
		}
	}
	if len(page.Rows) > 30 {
		page.Rows = page.Rows[:30]
		page.Next = page.Rows[29].ID
	}
	for _, count := range counts {
		page.Total += count
	}
	page.Completed = counts["applied"] + counts["skipped"] + counts["failed"]
	if page.Total > 0 {
		page.Percent = 100 * page.Completed / page.Total
	}
	s.summaryMu.RLock()
	page.SummaryError = s.summaryError
	s.summaryMu.RUnlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err = activityTemplate.Execute(w, page); err != nil {
		return
	}
}
