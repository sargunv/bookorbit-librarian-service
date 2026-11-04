package organize

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func TestBackupPreservesOriginalAndDetectsChanges(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	originals := filepath.Join(base, "originals")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "book.epub")
	if err := os.WriteFile(source, []byte("first edition"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := New(root, originals, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	b := domain.Book{ID: 42, Metadata: domain.Metadata{Title: "Example"}, Files: []domain.File{{ID: 9, AbsolutePath: source}}}
	if err = o.Backup(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if err = o.Backup(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(originals, "books", "42", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m backupManifest
	if err = json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Book.Metadata.Title != "Example" || len(m.Files) != 1 || m.Files[0].Source != source || m.Files[0].ID != 9 {
		t.Fatalf("bad manifest: %+v", m)
	}
	if err = os.WriteFile(source, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = o.Backup(context.Background(), b); err != nil {
		t.Fatalf("repeat backup after metadata change: %v", err)
	}
	copyData, err := os.ReadFile(filepath.Join(originals, "books", "42", m.Files[0].Copy))
	if err != nil || string(copyData) != "first edition" {
		t.Fatalf("original copy changed: %q %v", copyData, err)
	}
}

func TestPathPolicyRejectsEscapesAndSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "external"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root, filepath.Join(root, "backup"), ""); err == nil {
		t.Fatal("backup inside library accepted")
	}
	if err := os.Symlink(filepath.Join(base, "external"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{root, filepath.Join(root, "..", "external", "x"), filepath.Join(root, "link", "x")} {
		if err := checkPath(root, p, false); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
}

func TestEditionMatching(t *testing.T) {
	a := domain.Book{LibraryID: 1, Metadata: domain.Metadata{Title: "A Book", Language: "English", ISBN13: "978-123", Authors: []string{"A. Writer"}}}
	b := a
	b.Metadata.Title = "A BOOK"
	b.Metadata.ISBN13 = "978123"
	if !match(a, b) {
		t.Fatal("expected ISBN match")
	}
	b.Metadata.Language = "French"
	if match(a, b) {
		t.Fatal("different language matched")
	}
	b = a
	b.Metadata.Authors = []string{"Someone Else"}
	if match(a, b) {
		t.Fatal("different authors matched")
	}
	b = a
	b.LibraryID = 2
	if match(a, b) {
		t.Fatal("cross-library match")
	}
	b = a
	b.HardcoverEditionID = "2"
	a.HardcoverEditionID = "1"
	if match(a, b) {
		t.Fatal("conflicting editions matched by ISBN")
	}
	b.HardcoverEditionID = "1"
	b.Metadata.Title = "Different"
	if !match(a, b) {
		t.Fatal("same edition ID should match")
	}
	a.HardcoverEditionID = ""
	b.HardcoverEditionID = ""
	a.Metadata.ISBN13 = ""
	b.Metadata.ISBN13 = ""
	b.Metadata.Title = a.Metadata.Title
	if match(a, b) {
		t.Fatal("title alone matched")
	}
}

func TestDestinationClaimDoesNotOverwrite(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "source")
	dst := filepath.Join(d, "destination")
	if err := os.WriteFile(src, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveExclusive(src, dst); err == nil {
		t.Fatal("link overwrote existing file")
	}
	if data, err := os.ReadFile(src); err != nil || string(data) != "source" {
		t.Fatal("source changed on collision")
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "existing" {
		t.Fatal("destination changed")
	}
}
