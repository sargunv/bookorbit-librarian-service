package service

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sargunv/bookorbit-librarian-service/internal/bookorbit"
	"github.com/sargunv/bookorbit-librarian-service/internal/config"
	"github.com/sargunv/bookorbit-librarian-service/internal/curator"
	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
	"github.com/sargunv/bookorbit-librarian-service/internal/organize"
	"github.com/sargunv/bookorbit-librarian-service/internal/store"
)

type Service struct {
	cfg       config.Config
	db        *store.Store
	books     *bookorbit.Client
	judge     *curator.Curator
	organizer *organize.Organizer
	paused    atomic.Bool
	applyMu   sync.Mutex
	wg        sync.WaitGroup
}

func New(c config.Config) (*Service, error) {
	db, e := store.Open(c.StatePath)
	if e != nil {
		return nil, e
	}
	dsn := ""
	if c.Organize {
		dsn = c.PostgresDSN
	}
	org, e := organize.New(c.LibraryRoot, c.OriginalsRoot, dsn)
	if e != nil {
		_ = db.Close()
		return nil, e
	}
	judge := curator.New(c.ModelURL, c.ModelKey, c.PrimaryModel, c.FallbackModel)
	judge.SetRequestInterval(c.ModelInterval)
	return &Service{cfg: c, db: db, books: bookorbit.New(c.BookOrbitURL, c.BookOrbitToken), judge: judge, organizer: org}, nil
}

func (s *Service) Close() error { s.wg.Wait(); return errors.Join(s.organizer.Close(), s.db.Close()) }

func (s *Service) Start(ctx context.Context) {
	for i := 0; i < s.cfg.Workers; i++ {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.work(ctx) }()
	}
	if s.cfg.AutoDiscover {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				if !s.paused.Load() {
					if _, e := s.Discover(ctx, 0); e != nil {
						slog.Warn("discovery failed")
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(s.cfg.PollInterval):
				}
			}
		}()
	}
}

func (s *Service) Discover(ctx context.Context, limit int) (int, error) {
	ids, e := s.books.ListBooks(ctx, s.cfg.LibraryID)
	if e != nil {
		return 0, e
	}
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return s.db.Enqueue(ctx, ids, curator.PolicyVersion)
}

func (s *Service) work(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if s.paused.Load() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		j, e := s.db.Claim(ctx, s.cfg.JobTimeout+time.Minute, s.cfg.MaxAttempts)
		if errors.Is(e, sql.ErrNoRows) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}
		if e != nil {
			slog.Error("job claim failed")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		jobCtx, cancel := context.WithTimeout(ctx, s.cfg.JobTimeout)
		status, e := s.process(jobCtx, j)
		cancel()
		if e != nil {
			if ctx.Err() != nil {
				return
			}
			if x := s.db.Fail(ctx, j, e.Error(), s.cfg.MaxAttempts); x != nil {
				slog.Error("job failure persistence failed", "job", j.ID)
			}
			slog.Warn("job attempt failed", "job", j.ID, "attempt", j.Attempts)
		} else {
			slog.Info("job finished", "job", j.ID, "book", j.BookID, "status", status)
		}
	}
}

func (s *Service) finish(ctx context.Context, j store.Job, status, message string, result json.RawMessage) (string, error) {
	return status, s.db.Finish(ctx, j.ID, status, message, result)
}

func (s *Service) process(ctx context.Context, j store.Job) (string, error) {
	book, e := s.books.Book(ctx, j.BookID)
	if e != nil {
		if strings.Contains(e.Error(), "HTTP 404") {
			return s.finish(ctx, j, "skipped", "book entry no longer exists", nil)
		}
		return "", e
	}
	if book.LibraryID != s.cfg.LibraryID {
		return s.finish(ctx, j, "skipped", "book is outside configured library", nil)
	}
	p := j.Proposal
	if p == nil {
		candidates, e := s.books.Search(ctx, book, s.cfg.Providers)
		var warnings []string
		if e != nil {
			var partial *bookorbit.SearchError
			if len(candidates) == 0 || !errors.As(e, &partial) {
				return "", e
			}
			warnings = append(warnings, partial.Error())
		}
		if len(candidates) == 0 {
			return s.finish(ctx, j, "review", "no source candidates found", nil)
		}
		decision, e := s.judge.Judge(ctx, book, candidates)
		if e != nil {
			return "", e
		}
		proposal, e := curator.BuildProposal(book, candidates, decision)
		if e != nil {
			return "", e
		}
		p = &proposal
		p.Warnings = warnings
		if e = s.db.Proposal(ctx, j.ID, *p); e != nil {
			return "", e
		}
	}
	if p.Decision.NeedsReview || p.Decision.Match == 0 || !p.Decision.SameWork {
		return s.finish(ctx, j, "review", p.Decision.Reason, nil)
	}
	if !s.cfg.AutoApply {
		return s.finish(ctx, j, "planned", "", nil)
	}
	// Serialize side effects even when candidate gathering uses several workers.
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	current, e := s.books.Book(ctx, j.BookID)
	if e != nil {
		return "", e
	}
	already := patchMatches(current, p.Patch)
	if !already && !reflect.DeepEqual(current.Metadata, p.Book.Metadata) {
		return s.finish(ctx, j, "review", "metadata changed after evidence was gathered", nil)
	}
	if e = s.organizer.Backup(ctx, current); e != nil {
		return "", e
	}
	if !already && len(p.Patch) > 0 {
		if e = s.books.Apply(ctx, j.BookID, p.Patch); e != nil {
			return "", e
		}
	}
	if p.CoverURL != "" {
		if e = s.books.Cover(ctx, j.BookID, p.CoverURL); e != nil {
			return "", e
		}
	}
	if len(p.Patch) > 0 || p.CoverURL != "" {
		if e = s.books.Write(ctx, j.BookID); e != nil {
			return "", e
		}
	}
	result := json.RawMessage(`{}`)
	if s.cfg.Organize {
		fresh, e := s.books.Book(ctx, j.BookID)
		if e != nil {
			return "", e
		}
		// Fresh backup also covers alternate files if a previous retry grouped them.
		if e = s.organizer.Backup(ctx, fresh); e != nil {
			return "", e
		}
		r, e := s.organizer.Organize(ctx, fresh)
		if e != nil {
			return "", e
		}
		if len(r.MergedBookIDs) > 0 {
			if e = s.books.Write(ctx, j.BookID); e != nil {
				return "", e
			}
		}
		result, e = json.Marshal(r)
		if e != nil {
			return "", e
		}
	}
	return s.finish(ctx, j, "applied", "", result)
}

func patchMatches(b domain.Book, p domain.Patch) bool {
	data, e := json.Marshal(b.Metadata)
	if e != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	ids := map[string]string{"hardcover": "hardcoverId", "google": "googleBooksId", "goodreads": "goodreadsId", "amazon": "amazonId", "openLibrary": "openLibraryId", "itunes": "itunesId", "comicvine": "comicvineId", "kobo": "koboId"}
	for provider, key := range ids {
		if v := b.ProviderIDs[provider]; v != "" {
			fields[key], _ = json.Marshal(v)
		}
	}
	if b.HardcoverEditionID != "" {
		fields["hardcoverEditionId"], _ = json.Marshal(b.HardcoverEditionID)
	}
	for k, v := range p {
		var a, c json.RawMessage
		if json.Unmarshal(v, &a) != nil || json.Unmarshal(fields[k], &c) != nil {
			return false
		}
		if string(a) != string(c) {
			return false
		}
	}
	return true
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if s.db.Ping(ctx) != nil {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, 200, struct {
			Status string `json:"status"`
		}{"ok"})
	})
	private := http.NewServeMux()
	private.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		counts, e := s.db.Counts(r.Context())
		if e != nil {
			http.Error(w, "storage error", 500)
			return
		}
		writeJSON(w, 200, struct {
			Counts                              map[string]int `json:"jobs"`
			Paused                              bool           `json:"paused"`
			Policy, PrimaryModel, FallbackModel string
			AutoApply, Organize                 bool
		}{counts, s.paused.Load(), curator.PolicyVersion, s.cfg.PrimaryModel, s.cfg.FallbackModel, s.cfg.AutoApply, s.cfg.Organize})
	})
	private.HandleFunc("POST /v1/pause", func(w http.ResponseWriter, r *http.Request) {
		s.paused.Store(true)
		writeJSON(w, 200, struct {
			Paused bool `json:"paused"`
		}{true})
	})
	private.HandleFunc("POST /v1/resume", func(w http.ResponseWriter, r *http.Request) {
		s.paused.Store(false)
		writeJSON(w, 200, struct {
			Paused bool `json:"paused"`
		}{false})
	})
	private.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		jobs, e := s.db.List(r.Context(), r.URL.Query().Get("status"))
		if e != nil {
			http.Error(w, "storage error", 500)
			return
		}
		writeJSON(w, 200, jobs)
	})
	private.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil {
			http.Error(w, "invalid job id", 400)
			return
		}
		j, e := s.db.Get(r.Context(), id)
		if e != nil {
			http.Error(w, "job not found", 404)
			return
		}
		writeJSON(w, 200, j)
	})
	private.HandleFunc("POST /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			BookIDs []int `json:"bookIds"`
			Limit   int   `json:"limit"`
		}
		if !decode(w, r, &in) {
			return
		}
		var n int
		var e error
		if len(in.BookIDs) > 0 {
			if len(in.BookIDs) > 10000 {
				http.Error(w, "too many book IDs", 400)
				return
			}
			n, e = s.db.Enqueue(r.Context(), in.BookIDs, curator.PolicyVersion)
		} else {
			n, e = s.Discover(r.Context(), in.Limit)
		}
		if e != nil {
			http.Error(w, "enqueue failed", http.StatusBadGateway)
			return
		}
		writeJSON(w, 202, struct {
			Enqueued int `json:"enqueued"`
		}{n})
	})
	private.HandleFunc("POST /v1/jobs/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil {
			http.Error(w, "invalid job id", 400)
			return
		}
		if e = s.db.Retry(r.Context(), id); e != nil {
			http.Error(w, "job not retryable", http.StatusConflict)
			return
		}
		writeJSON(w, 202, struct {
			Status string `json:"status"`
		}{"pending"})
	})
	mux.Handle("/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.AdminToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		private.ServeHTTP(w, r)
	}))
	return mux
}

func writeJSON[T interface{}](w http.ResponseWriter, status int, value T) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if e := json.NewEncoder(w).Encode(value); e != nil {
		slog.Warn("response encoding failed")
	}
}

func decode[T interface{}](w http.ResponseWriter, r *http.Request, dst *T) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(dst); e != nil {
		http.Error(w, "invalid request JSON", 400)
		return false
	}
	return true
}
