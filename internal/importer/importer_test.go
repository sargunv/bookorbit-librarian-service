package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDurableImportDedupAndRecovery(t *testing.T) {
	root := t.TempDir()
	m, e := New(root, "unused", "unused", "unused", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	j, e := m.Create("../../Example.epub", strings.NewReader("example bytes"))
	if e != nil {
		t.Fatal(e)
	}
	if j.Name != "Example.epub" || j.Status != "queued" {
		t.Fatalf("bad job: %+v", j)
	}
	duplicate, e := m.Create("Other.epub", strings.NewReader("example bytes"))
	if e != nil || duplicate.ID != j.ID {
		t.Fatal("duplicate not reused", e)
	}
	j.Status = "failed"
	j.Session = "private-upstream-id"
	j.BookID = 42
	if e = m.save(j); e != nil {
		t.Fatal(e)
	}
	restarted, e := New(root, "unused", "unused", "unused", nil, func(context.Context, int) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	if e = restarted.Retry(j.ID); e != nil {
		t.Fatal(e)
	}
	jobs, e := restarted.jobs()
	if e != nil || len(jobs) != 1 {
		t.Fatal(e)
	}
	if jobs[0].Session != j.Session {
		t.Fatal("lost session recovery")
	}
	restarted.process(context.Background(), jobs[0])
	jobs, e = restarted.jobs()
	if e != nil || jobs[0].Status != "completed" {
		t.Fatal("existing imported book did not resume", e)
	}
	info, e := os.Stat(filepath.Join(root, j.ID, "job.json"))
	if e != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("unsafe state mode", e)
	}
	data, e := json.Marshal(jobs)
	if e != nil || bytes.Contains(data, []byte("private-upstream-id")) {
		t.Fatal("session leaked", e)
	}
}

func TestImportMutationBoundary(t *testing.T) {
	m, e := New(t.TempDir(), "unused", "unused", "unused", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	m.Mount(mux, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	})
	for _, tc := range []struct {
		auth, header, origin string
		status               int
	}{{"", "", "", 401}, {"Bearer test", "", "", 403}, {"Bearer test", "1", "https://evil.invalid", 403}, {"Bearer test", "1", "http://example.com", 400}} {
		req := httptest.NewRequest("POST", "http://example.com/imports/upload", strings.NewReader("not multipart"))
		req.Header.Set("Authorization", tc.auth)
		req.Header.Set("X-Librarian-Import", tc.header)
		req.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("got %d want %d", w.Code, tc.status)
		}
	}
	if _, e = m.Create("test.exe", strings.NewReader("bad")); e == nil {
		t.Fatal("unsupported file accepted")
	}
}
