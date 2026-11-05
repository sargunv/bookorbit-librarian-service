package service

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sargunv/bookorbit-librarian-service/internal/config"
	"github.com/sargunv/bookorbit-librarian-service/internal/curator"
	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func TestUIRequiresSeparateCapabilityAndEscapesModelText(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{StatePath: filepath.Join(dir, "events.db"), LibraryRoot: filepath.Join(dir, "books"), OriginalsRoot: filepath.Join(dir, "originals"), AdminToken: strings.Repeat("a", 32), UIProxyToken: strings.Repeat("b", 32)}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Close() }()
	if _, err = svc.db.Enqueue(context.Background(), []int{1}, curator.PolicyVersion); err != nil {
		t.Fatal(err)
	}
	job, err := svc.db.Claim(context.Background(), time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.db.Proposal(context.Background(), job.ID, domain.Proposal{Book: domain.Book{Metadata: domain.Metadata{Title: "<script>alert(1)</script>"}}}); err != nil {
		t.Fatal(err)
	}
	if err = svc.db.Finish(context.Background(), job.ID, "applied", "", nil); err != nil {
		t.Fatal(err)
	}
	if err = svc.db.SaveSummary(context.Background(), domain.ActivitySummary{Policy: curator.PolicyVersion, ToEvent: 1, Highlights: []domain.SummaryHighlight{{Text: "<script>alert(1)</script><img src=x onerror=alert(2)>", EventIDs: []int64{1}}}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, key, value string
		status           int
	}{
		{"/", "Remote-User", "someone", 401},
		{"/covers/1/current", "Remote-User", "someone", 401},
		{"/", "X-Librarian-UI-Token", cfg.UIProxyToken, 200},
		{"/v1/status", "X-Librarian-UI-Token", cfg.UIProxyToken, 401},
		{"/", "Authorization", "Bearer " + cfg.AdminToken, 200},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set(tc.key, tc.value)
		w := httptest.NewRecorder()
		svc.Handler().ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s with %s: %d", tc.path, tc.key, w.Code)
		}
		if tc.status == 200 && !strings.Contains(w.Body.String(), "&lt;script&gt;") {
			t.Fatal("escaped event missing from rendered dashboard")
		}
		if strings.Contains(w.Body.String(), "<script>alert") || strings.Contains(w.Body.String(), cfg.UIProxyToken) || strings.Contains(w.Body.String(), cfg.AdminToken) {
			t.Fatal("unescaped model content or exposed credential")
		}
	}
}
