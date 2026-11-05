package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sargunv/bookorbit-librarian-service/internal/bookorbit"
	"github.com/sargunv/bookorbit-librarian-service/internal/config"
	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
	"github.com/sargunv/bookorbit-librarian-service/internal/store"
)

func TestCurationUsesDiscoveredGroupDespiteMissingOrWrongSeries(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/magic-links/login":
			_, _ = fmt.Fprint(w, `{"accessToken":"token"}`)
		case "/api/v1/books/2":
			_, _ = fmt.Fprint(w, `{"id":2,"libraryId":1,"title":"Second Voyage","seriesName":"Different spelling"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	svc := Service{cfg: config.Config{LibraryID: 1}, db: db, books: bookorbit.New(server.URL, "key"), discovery: &domain.Discovery{Groups: []domain.Group{{BookIDs: []int{1, 2}, Reason: "related"}, {BookIDs: []int{3}}}}}
	members, err := svc.groupMembers(context.Background(), domain.Book{ID: 1, LibraryID: 1, Metadata: domain.Metadata{Title: "First Voyage"}})
	if err != nil || len(members) != 2 || members[1].ID != 2 {
		t.Fatalf("members: %+v %v", members, err)
	}
}
