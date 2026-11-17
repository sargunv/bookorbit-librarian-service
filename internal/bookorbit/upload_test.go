package bookorbit

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUploadResumesSavedOffsetAndCompletedSession(t *testing.T) {
	saved := false
	completed := false
	chunkCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/magic-links/login":
			_, _ = fmt.Fprint(w, `{"accessToken":"test"}`)
		case "/api/v1/uploads":
			t.Error("must resume, not create another upload")
			w.WriteHeader(500)
		case "/api/v1/uploads/session":
			if completed {
				_, _ = fmt.Fprint(w, `{"id":"session","status":"completed","bookId":42}`)
			} else {
				_, _ = fmt.Fprint(w, `{"id":"session","status":"receiving","receivedBytes":3,"chunkSizeBytes":10}`)
			}
		case "/api/v1/uploads/session/chunks":
			chunkCalls++
			if r.Header.Get("Upload-Offset") != "3" {
				t.Error("incorrect offset")
			}
			reader, e := r.MultipartReader()
			if e != nil {
				t.Error(e)
				return
			}
			part, e := reader.NextPart()
			if e != nil {
				t.Error(e)
				return
			}
			data, e := io.ReadAll(part)
			if e != nil || string(data) != "def" {
				t.Error("incorrect resumed bytes", e)
			}
			_, _ = fmt.Fprint(w, `{"id":"session","status":"receiving","receivedBytes":6,"chunkSizeBytes":10}`)
		case "/api/v1/uploads/session/complete":
			completed = true
			_, _ = fmt.Fprint(w, `{"id":"session","status":"completed","bookId":42}`)
		default:
			t.Error("unexpected request")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "book.epub")
	if e := os.WriteFile(path, []byte("abcdef"), 0o600); e != nil {
		t.Fatal(e)
	}
	c := New(server.URL, "test")
	for range 2 {
		id, e := c.UploadFile(context.Background(), "key", 1, path, "Example.epub", "session", func(string) error { saved = true; return nil })
		if e != nil || id != 42 {
			t.Fatal("upload failed", e)
		}
	}
	if saved || chunkCalls != 1 {
		t.Fatal("completed session repeated upload")
	}
}
