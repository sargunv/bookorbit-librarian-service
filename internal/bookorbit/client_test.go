package bookorbit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func TestDetailAndAuthenticationRenewal(t *testing.T) {
	var mu sync.Mutex
	logins := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/auth/magic-links/login":
			if r.Header.Get("Authorization") != "" {
				t.Error("credentials on login")
			}
			var v map[string]string
			if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
				t.Fatal(err)
			}
			if v["token"] != "magic-secret" {
				t.Error("wrong login token")
			}
			logins++
			_, _ = fmt.Fprintf(w, `{"accessToken":"access-%d"}`, logins)
		case "/api/v1/books/42":
			if r.Header.Get("Authorization") == "Bearer access-1" {
				w.WriteHeader(401)
				return
			}
			if r.Header.Get("Authorization") != "Bearer access-2" {
				t.Error("wrong access token")
			}
			_, _ = fmt.Fprint(w, `{"id":42,"libraryId":7,"title":"Example","authors":[{"id":9,"name":"A Writer"}],"genres":["Fiction"],"updatedAt":null,"publisher":null,"providerIds":{"google":"abc","hardcover":null},"hardcoverEditionId":"ed","lockedFields":["title"],"files":[{"id":2,"format":"epub","role":"primary","absolutePath":"/books/a","sizeBytes":123}]}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := New(server.URL+"/api/v1/", "magic-secret")
	b, err := c.Book(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if logins != 2 || b.ID != 42 || b.LibraryID != 7 || b.Metadata.Title != "Example" || len(b.Metadata.Authors) != 1 || b.Metadata.Authors[0] != "A Writer" || b.ProviderIDs["google"] != "abc" || b.HardcoverEditionID != "ed" || len(b.Files) != 1 || b.Files[0].SizeBytes != 123 || len(b.LockedFields) != 1 {
		t.Fatalf("unexpected detail: %+v", b)
	}
}

func TestRedirectDoesNotLeakCredentials(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer other.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer s.Close()
	_, err := New(s.URL, "secret").Book(context.Background(), 1)
	if err == nil || hit {
		t.Fatalf("redirect followed or no error: %v, hit=%v", err, hit)
	}
}

func TestSearchSSEPartialAndFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			_, _ = fmt.Fprint(w, `{"accessToken":"token"}`)
			return
		}
		q := r.URL.Query()
		if q.Get("providers") != "google,openLibrary" || q.Get("title") != "Example" || q.Get("author") != "Writer" || q.Get("isbn") != "123" || q.Get("bookId") != "8" || q.Get("resultsPerProvider") != "5" {
			t.Errorf("bad search query: %v", q)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, ": heartbeat\r\nevent: message\r\ndata: {\"provider\":\"google\",\r\ndata: \"providerId\":\"a\",\"title\":\"Result\"}\r\n\r\nevent: provider-status\r\ndata: {\"provider\":\"openLibrary\",\"outcome\":\"timeout\"}\r\n\r\n")
	}))
	defer s.Close()
	b := domain.Book{ID: 8, Metadata: domain.Metadata{Title: "Example", Authors: []string{"Writer"}, ISBN13: "123"}}
	candidates, err := New(s.URL, "secret").search(context.Background(), b, []string{"google", "openLibrary"}, true)
	status, ok := err.(*SearchError)
	if !ok || status.Statuses["openLibrary"] != "timeout" || len(candidates) != 1 || candidates[0].Title != "Result" {
		t.Fatalf("got %#v, %v", candidates, err)
	}
}

func TestPaginationAndWriteFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/magic-links/login":
			_, _ = fmt.Fprint(w, `{"accessToken":"token"}`)
		case "/api/v1/books/query":
			var v struct {
				Pagination struct {
					Page int `json:"page"`
				} `json:"pagination"`
			}
			if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
				t.Fatal(err)
			}
			if v.Pagination.Page == 0 {
				_, _ = fmt.Fprint(w, `{"total":201,"items":[{"id":1,"libraryId":5}]}`)
			} else {
				_, _ = fmt.Fprint(w, `{"total":201,"items":[{"id":2,"libraryId":6},{"id":3,"libraryId":5}]}`)
			}
		case "/api/v1/books/1/metadata":
			if r.URL.Query().Get("syncFileWrite") != "true" {
				t.Error("missing sync")
			}
			_, _ = fmt.Fprint(w, `{"book":{},"write":{"status":"failed"}}`)
		case "/api/v1/books/1":
			_, _ = fmt.Fprint(w, `{"id":1,"title":"Example"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	c := New(s.URL, "secret")
	ids, err := c.ListBooks(context.Background(), 5)
	if err != nil || len(ids) != 2 || ids[1] != 3 {
		t.Fatalf("ids %v error %v", ids, err)
	}
	if c.Apply(context.Background(), 1, domain.Patch{}) == nil {
		t.Error("missed write failure")
	}
	if c.Write(context.Background(), 1) == nil {
		t.Error("missed file write failure")
	}
}

func TestSearchEscapesWrongStoredIdentifiers(t *testing.T) {
	searches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/login") {
			_, _ = fmt.Fprint(w, `{"accessToken":"token"}`)
			return
		}
		searches++
		q := r.URL.Query()
		if q.Get("title") != "A Book" || q.Get("author") != "Writer" {
			t.Error("missing title/author")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if searches == 1 {
			if q.Get("bookId") != "8" || q.Get("isbn") != "wrong" {
				t.Error("missing linked lookup")
			}
			_, _ = fmt.Fprint(w, "data: {\"provider\":\"hardcover\",\"providerId\":\"old\",\"title\":\"Translation\",\"language\":\"rus\"}\n\n")
		} else {
			if q.Has("bookId") || q.Has("isbn") {
				t.Error("fresh search pinned to stored identity")
			}
			_, _ = fmt.Fprint(w, "data: {\"provider\":\"hardcover\",\"providerId\":\"new\",\"title\":\"A Book\",\"language\":\"eng\"}\n\n")
		}
	}))
	defer server.Close()
	cs, err := New(server.URL, "key").Search(context.Background(), domain.Book{ID: 8, Metadata: domain.Metadata{Title: "A Book", Authors: []string{"Writer"}, ISBN13: "wrong"}}, []string{"hardcover"})
	if err != nil || searches != 2 || len(cs) != 2 || cs[1].ProviderID != "new" {
		t.Fatalf("independent evidence missing: %+v %v", cs, err)
	}
}
