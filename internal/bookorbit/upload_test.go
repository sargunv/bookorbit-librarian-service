package bookorbit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func uploadFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.epub")
	if e := os.WriteFile(path, []byte("abcdef"), 0o600); e != nil {
		t.Fatal(e)
	}
	return path
}

// readUpload returns the single multipart file part's name and bytes.
func readUpload(t *testing.T, r *http.Request) (string, string) {
	t.Helper()
	reader, e := r.MultipartReader()
	if e != nil {
		t.Error(e)
		return "", ""
	}
	part, e := reader.NextPart()
	if e != nil {
		t.Error(e)
		return "", ""
	}
	data, e := io.ReadAll(part)
	if e != nil {
		t.Error(e)
	}
	return part.FileName(), string(data)
}

func TestUploadMarksPendingBeforeSendingAndReturnsBook(t *testing.T) {
	state := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/magic-links/login":
			_, _ = fmt.Fprint(w, `{"accessToken":"test"}`)
		case "/api/v1/libraries/7/upload":
			if state != UploadPending {
				t.Error("attempt not recorded before sending")
			}
			if name, data := readUpload(t, r); name != "Example.epub" || data != "abcdef" {
				t.Error("incorrect upload", name, data)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"bookId":42,"filename":"Example.epub","format":"epub","sizeBytes":6}`)
		default:
			t.Error("unexpected request", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	id, e := New(server.URL, "test").UploadFile(context.Background(), 7, uploadFixture(t), "Example.epub", "", func(s string) error { state = s; return nil })
	if e != nil || id != 42 {
		t.Fatal("upload failed", e)
	}
}

func TestUploadRefusesRecordedState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not contact BookOrbit", r.URL.Path)
	}))
	defer server.Close()
	c := New(server.URL, "test")
	for _, state := range []string{UploadPending, "legacy-session-id"} {
		_, e := c.UploadFile(context.Background(), 1, uploadFixture(t), "Example.epub", state, func(string) error {
			t.Error("must not change recorded state")
			return nil
		})
		if !errors.Is(e, ErrUploadUnknown) {
			t.Fatal("expected unknown outcome", e)
		}
	}
}

func TestUploadReauthenticatesWithFullBody(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/magic-links/login":
			_, _ = fmt.Fprintf(w, `{"accessToken":"token-%d"}`, calls)
		case "/api/v1/libraries/1/upload":
			calls++
			if _, data := readUpload(t, r); data != "abcdef" {
				t.Error("incomplete replay", data)
			}
			if calls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer token-1" {
				t.Error("stale token")
			}
			_, _ = fmt.Fprint(w, `{"bookId":5}`)
		}
	}))
	defer server.Close()
	id, e := New(server.URL, "test").UploadFile(context.Background(), 1, uploadFixture(t), "Example.epub", "", func(string) error { return nil })
	if e != nil || id != 5 || calls != 2 {
		t.Fatal("reauthentication failed", e, calls)
	}
}

func TestUploadRejectionClearsPendingAndLostResponseKeepsIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply func(http.ResponseWriter)
		state string
	}{
		{"rejected", func(w http.ResponseWriter) { w.WriteHeader(http.StatusConflict) }, ""},
		{"lost", func(w http.ResponseWriter) {
			conn, _, e := w.(http.Hijacker).Hijack()
			if e == nil {
				_ = conn.Close()
			}
		}, UploadPending},
		{"no book", func(w http.ResponseWriter) { _, _ = fmt.Fprint(w, `{}`) }, UploadPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/auth/magic-links/login" {
					_, _ = fmt.Fprint(w, `{"accessToken":"test"}`)
					return
				}
				readUpload(t, r)
				tc.reply(w)
			}))
			defer server.Close()
			state := ""
			_, e := New(server.URL, "test").UploadFile(context.Background(), 1, uploadFixture(t), "Example.epub", "", func(s string) error { state = s; return nil })
			if e == nil || state != tc.state {
				t.Fatalf("state %q, error %v", state, e)
			}
		})
	}
}
