package service

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
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
	cfg          config.Config
	db           *store.Store
	books        *bookorbit.Client
	judge        *curator.Curator
	organizer    *organize.Organizer
	paused       atomic.Bool
	applyMu      sync.Mutex
	planMu       sync.Mutex
	discoveryMu  sync.Mutex
	discovery    *domain.Discovery
	summaryMu    sync.RWMutex
	summaryError string
	wg           sync.WaitGroup
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
	judge := curator.New(c.ModelURL, c.ModelKey, c.VisionModel, c.MetadataModel)
	judge.SetRequestInterval(c.ModelInterval)
	return &Service{cfg: c, db: db, books: bookorbit.New(c.BookOrbitURL, c.BookOrbitToken), judge: judge, organizer: org}, nil
}

func (s *Service) Close() error { s.wg.Wait(); return errors.Join(s.organizer.Close(), s.db.Close()) }

func (s *Service) Start(ctx context.Context) {
	if s.cfg.UIProxyToken != "" {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.summarizeActivity(ctx) }()
	}
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
	if _, e = s.discoverGroups(ctx, ids, false); e != nil {
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
	s.progress(ctx, j, book.Metadata.Title, "started", 0, 0)
	ctx = curator.WithProgress(ctx, func(stage string, done, total int) { s.progress(ctx, j, book.Metadata.Title, stage, done, total) })
	p := j.Proposal
	if p == nil {
		s.planMu.Lock()
		saved, err := s.db.Get(ctx, j.ID)
		if err == nil && saved.Proposal != nil {
			p = saved.Proposal
		} else if err == nil {
			s.progress(ctx, j, book.Metadata.Title, "grouping", 0, 0)
			members, x := s.groupMembers(ctx, book)
			if x == nil {
				evidence := make([]domain.Evidence, 0, len(members))
				for index, member := range members {
					s.progress(ctx, j, book.Metadata.Title, "sources", index+1, len(members))
					candidates, searchErr := s.books.Search(ctx, member, s.cfg.Providers)
					var warnings []string
					if searchErr != nil {
						var partial *bookorbit.SearchError
						if len(candidates) == 0 || !errors.As(searchErr, &partial) {
							x = searchErr
							break
						}
						warnings = append(warnings, partial.Error())
					}
					evidence = append(evidence, domain.Evidence{Book: member, Candidates: candidates, Warnings: warnings})
				}
				if x == nil {
					proposals, planErr := s.judge.Plan(ctx, evidence, s.books.CoverImage)
					x = planErr
					if x == nil {
						x = s.db.SaveGroup(ctx, proposals)
						for i := range proposals {
							if proposals[i].Book.ID == j.BookID {
								p = &proposals[i]
							}
						}
					}
				}
			}
			err = x
		}
		s.planMu.Unlock()
		if err != nil {
			return "", err
		}
		if p == nil {
			return "", errors.New("series plan omitted current book")
		}
	}
	if p.Decision.Match == 0 {
		return s.finish(ctx, j, "skipped", p.Decision.Reason, nil)
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
	// Rebuild against the fresh record on retries: BookOrbit normalizes some
	// fields while writing, and those own changes must not create review holds.
	refreshed, err := curator.BuildProposal(current, p.Candidates, p.Decision)
	if err != nil {
		return "", err
	}
	p.Patch = refreshed.Patch
	p.CoverURL = refreshed.CoverURL
	s.progress(ctx, j, book.Metadata.Title, "applying", 0, 0)
	if e = s.organizer.Backup(ctx, current); e != nil {
		return "", e
	}
	if len(p.Patch) > 0 {
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
		s.progress(ctx, j, book.Metadata.Title, "organizing", 0, 0)
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
	s.mountUI(mux)
	private := http.NewServeMux()
	private.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		counts, e := s.db.Counts(r.Context())
		if e != nil {
			http.Error(w, "storage error", 500)
			return
		}
		writeJSON(w, 200, struct {
			Counts                             map[string]int `json:"jobs"`
			Paused                             bool           `json:"paused"`
			Policy, VisionModel, MetadataModel string
			AutoApply, Organize                bool
		}{counts, s.paused.Load(), curator.PolicyVersion, s.cfg.VisionModel, s.cfg.MetadataModel, s.cfg.AutoApply, s.cfg.Organize})
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
	private.HandleFunc("GET /v1/discovery", func(w http.ResponseWriter, r *http.Request) {
		plan, err := s.db.Discovery(r.Context(), curator.PolicyVersion)
		if err != nil {
			http.Error(w, "discovery not available", 404)
			return
		}
		writeJSON(w, 200, plan)
	})
	private.HandleFunc("POST /v1/discovery", func(w http.ResponseWriter, r *http.Request) {
		ids, err := s.books.ListBooks(r.Context(), s.cfg.LibraryID)
		if err != nil {
			http.Error(w, "inventory failed", http.StatusBadGateway)
			return
		}
		plan, err := s.discoverGroups(r.Context(), ids, true)
		if err != nil {
			http.Error(w, "discovery failed", http.StatusBadGateway)
			return
		}
		writeJSON(w, 200, plan)
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
