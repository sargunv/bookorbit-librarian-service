package service

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sargunv/bookorbit-librarian-service/internal/config"
)

// TestBrowserImport optionally exercises the actual page, scripts and CSP in
// Chromium. Set LIBRARIAN_BROWSER_TEST to the browser smoke script's path.
func TestBrowserImport(t *testing.T) {
	script := os.Getenv("LIBRARIAN_BROWSER_TEST")
	if script == "" {
		t.Skip("set LIBRARIAN_BROWSER_TEST to run Chromium upload smoke test")
	}
	dir := t.TempDir()
	cfg := config.Config{StatePath: filepath.Join(dir, "events.db"), LibraryRoot: filepath.Join(dir, "books"), OriginalsRoot: filepath.Join(dir, "originals"), ImportRoot: filepath.Join(dir, "imports"), UIProxyToken: strings.Repeat("b", 32)}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Close() }()
	handler := svc.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Librarian-UI-Token", cfg.UIProxyToken)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	// The background worker stays stopped: uploaded fixtures remain in temporary
	// job storage and never contact BookOrbit, Adobe or model services.
	command := exec.Command("node", script, server.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("browser smoke: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}
