// Package organize backs up and reorganizes BookOrbit catalog files.
package organize

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

type Result struct {
	MovedFileCount int   `json:"movedFileCount"`
	MergedBookIDs  []int `json:"mergedBookIds"`
}
type Organizer struct {
	root, originals string
	db              *sql.DB
	mu              sync.Mutex
}

type move struct {
	ID   int    `json:"fileId"`
	From string `json:"from"`
	To   string `json:"to"`
	Hash string `json:"sha256"`
}
type record struct {
	book       domain.Book
	folderID   int
	folderRoot string
	folder     string
}

func New(root, originalsRoot, postgresDSN string) (*Organizer, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	originalsRoot, err = filepath.Abs(originalsRoot)
	if err != nil {
		return nil, err
	}
	if root == string(filepath.Separator) || within(root, originalsRoot) || within(originalsRoot, root) {
		return nil, errors.New("library and originals roots must be distinct, non-overlapping directories")
	}
	if err = checkPath(root, root, true); err != nil {
		return nil, fmt.Errorf("root: %w", err)
	}
	if err = checkPath(originalsRoot, originalsRoot, true); err != nil {
		return nil, fmt.Errorf("originals root: %w", err)
	}
	if err = os.MkdirAll(originalsRoot, 0o700); err != nil {
		return nil, err
	}
	o := &Organizer{root: root, originals: originalsRoot}
	if postgresDSN != "" {
		o.db, err = sql.Open("pgx", postgresDSN)
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (o *Organizer) Close() error {
	if o.db != nil {
		return o.db.Close()
	}
	return nil
}

func within(root, p string) bool {
	rel, e := filepath.Rel(root, p)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkPath rejects symlinks in all existing components, including ancestors of root.
func checkPath(root, p string, allowRoot bool) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || !within(root, p) || (!allowRoot && p == root) {
		return fmt.Errorf("path outside root or root itself: %q", p)
	}
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(p, cur), cur) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, e := os.Lstat(cur)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component: %s", cur)
		}
	}
	return nil
}

func mkdirSafe(root, p string) error {
	if err := checkPath(root, p, true); err != nil {
		return err
	}
	if err := os.MkdirAll(p, 0o700); err != nil {
		return err
	}
	return checkPath(root, p, true)
}

func syncDir(p string) error {
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func writeJSON[T interface{}](path string, v T) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if e = f.Chmod(0o600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	if x := f.Close(); e == nil {
		e = x
	}
	if e != nil {
		return e
	}
	if e = os.Rename(tmp, path); e != nil {
		return e
	}
	return syncDir(filepath.Dir(path))
}

func hashFile(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer func() { _ = f.Close() }()
	st, e := f.Stat()
	if e != nil {
		return "", e
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", path)
	}
	h := sha256.New()
	_, e = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), e
}

type backupManifest struct {
	Book  domain.Book  `json:"book"`
	Files []backupFile `json:"files"`
}
type backupFile struct {
	ID     int    `json:"fileId"`
	Source string `json:"sourcePath"`
	Copy   string `json:"copy"`
	Hash   string `json:"sha256"`
}

func (o *Organizer) Backup(ctx context.Context, book domain.Book) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.backup(ctx, book)
}

func (o *Organizer) backup(ctx context.Context, b domain.Book) error {
	if b.ID <= 0 {
		return errors.New("invalid book ID")
	}
	dir := filepath.Join(o.originals, "books", strconv.Itoa(b.ID))
	if e := mkdirSafe(o.originals, dir); e != nil {
		return e
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if e := checkPath(o.originals, manifestPath, false); e != nil {
		return e
	}
	var old backupManifest
	if data, e := os.ReadFile(manifestPath); e == nil {
		if e = json.Unmarshal(data, &old); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	byID := map[int]backupFile{}
	for _, f := range old.Files {
		byID[f.ID] = f
	}
	if old.Book.ID != 0 && old.Book.ID != b.ID {
		return errors.New("backup manifest book mismatch")
	}
	if old.Book.ID == 0 {
		old.Book = b
	}
	for _, f := range b.Files {
		if e := ctx.Err(); e != nil {
			return e
		}
		if f.ID <= 0 {
			return errors.New("invalid file ID")
		}
		if e := checkPath(o.root, f.AbsolutePath, false); e != nil {
			return e
		}
		h, e := hashFile(f.AbsolutePath)
		if e != nil {
			return e
		}
		if prev, ok := byID[f.ID]; ok {
			if filepath.Base(prev.Copy) != prev.Copy {
				return errors.New("invalid backup manifest path")
			}
			p := filepath.Join(dir, prev.Copy)
			if e = checkPath(o.originals, p, false); e != nil {
				return e
			}
			existing, e := hashFile(p)
			if e != nil || existing != prev.Hash {
				return fmt.Errorf("backup mismatch for file %d", f.ID)
			}
			continue
		}
		name := strconv.Itoa(f.ID) + "-" + h
		dest := filepath.Join(dir, name)
		if e = checkPath(o.originals, dest, false); e != nil {
			return e
		}
		in, e := os.Open(f.AbsolutePath)
		if e != nil {
			return e
		}
		tmp, e := os.CreateTemp(dir, ".backup-")
		if e != nil {
			_ = in.Close()
			return e
		}
		tmpName := tmp.Name()
		_, e = io.Copy(tmp, in)
		_ = in.Close()
		if e == nil {
			e = tmp.Sync()
		}
		if x := tmp.Close(); e == nil {
			e = x
		}
		if e == nil {
			var actual string
			actual, e = hashFile(tmpName)
			if e == nil && actual != h {
				e = errors.New("source changed while backing up")
			}
		}
		if e == nil {
			e = os.Link(tmpName, dest)
			if errors.Is(e, os.ErrExist) {
				var actual string
				actual, e = hashFile(dest)
				if e == nil && actual != h {
					e = errors.New("backup collision")
				}
			}
		}
		_ = os.Remove(tmpName)
		if e != nil {
			return e
		}
		if e = syncDir(dir); e != nil {
			return e
		}
		old.Files = append(old.Files, backupFile{f.ID, f.AbsolutePath, name, h})
		if e = writeJSON(manifestPath, old); e != nil {
			return e
		}
	}
	if len(b.Files) == 0 && old.Book.ID != 0 {
		return writeJSON(manifestPath, old)
	}
	return nil
}

func component(s, fallback string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
	s = strings.Trim(s, ". ")
	if s == "" {
		s = fallback
	}
	r := []rune(s)
	if len(r) > 100 {
		s = string(r[:100])
	}
	return s
}

func (o *Organizer) records(ctx context.Context, tx *sql.Tx, library int) ([]record, error) {
	rows, e := tx.QueryContext(ctx, `SELECT b.id,b.library_id,b.library_folder_id,lf.path,b.folder_path,coalesce(m.title,''),coalesce(m.subtitle,''),coalesce(m.description,''),coalesce(m.publisher,''),coalesce(m.published_date::text,''),coalesce(m.published_year,0),coalesce(m.page_count,0),coalesce(m.series_index,''),coalesce(m.series_name,''),coalesce(m.language,''),coalesce(m.isbn10,''),coalesce(m.isbn13,''),coalesce(m.hardcover_edition_id,''),coalesce((SELECT string_agg(a.name, E'\x1f' ORDER BY ba.display_order,a.id) FROM book_authors ba JOIN authors a ON a.id=ba.author_id WHERE ba.book_id=b.id),''),(jsonb_strip_nulls(jsonb_build_object('hardcover',m.hardcover_id,'goodreads',m.goodreads_id,'google',m.google_books_id,'openlibrary',m.open_library_id)) || coalesce((SELECT jsonb_object_agg(provider,external_id) FROM book_provider_ids WHERE book_id=b.id),'{}'::jsonb))::text,coalesce(array_to_json(m.locked_fields)::text,'[]'),coalesce((SELECT json_agg(g.name ORDER BY g.name)::text FROM book_genres bg JOIN genres g ON g.id=bg.genre_id WHERE bg.book_id=b.id),'[]') FROM books b JOIN library_folders lf ON lf.id=b.library_folder_id LEFT JOIN book_metadata m ON m.book_id=b.id WHERE b.library_id=$1 ORDER BY b.id FOR UPDATE OF b`, library)
	if e != nil {
		return nil, e
	}
	defer func() { _ = rows.Close() }()
	var out []record
	for rows.Next() {
		var r record
		var authors, providers, locks, genres string
		m := &r.book.Metadata
		if e = rows.Scan(&r.book.ID, &r.book.LibraryID, &r.folderID, &r.folderRoot, &r.folder, &m.Title, &m.Subtitle, &m.Description, &m.Publisher, &m.PublishedDate, &m.PublishedYear, &m.PageCount, &m.SeriesIndex, &m.SeriesName, &m.Language, &m.ISBN10, &m.ISBN13, &r.book.HardcoverEditionID, &authors, &providers, &locks, &genres); e != nil {
			return nil, e
		}
		if authors != "" {
			m.Authors = strings.Split(authors, "\x1f")
		}
		r.book.LibraryFolderID = r.folderID
		if e = json.Unmarshal([]byte(providers), &r.book.ProviderIDs); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(locks), &r.book.LockedFields); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(genres), &m.Genres); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func files(ctx context.Context, tx *sql.Tx, r *record) error {
	rows, e := tx.QueryContext(ctx, `SELECT id,absolute_path,coalesce(format,''),role,coalesce(file_hash,''),coalesce(size_bytes,0) FROM book_files WHERE book_id=$1 ORDER BY id FOR UPDATE`, r.book.ID)
	if e != nil {
		return e
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var f domain.File
		if e = rows.Scan(&f.ID, &f.AbsolutePath, &f.Format, &f.Role, &f.FileHash, &f.SizeBytes); e != nil {
			return e
		}
		r.book.Files = append(r.book.Files, f)
	}
	return rows.Err()
}

type operation struct {
	Library   int     `json:"libraryId"`
	Survivor  int     `json:"survivor"`
	OldFolder string  `json:"oldFolder"`
	NewFolder string  `json:"newFolder"`
	Moves     []entry `json:"moves"`
}
type entry struct {
	move
	OldBook int `json:"oldBookId"`
}

func (o *Organizer) recover(ctx context.Context) error {
	dir := filepath.Join(o.originals, "journals")
	if e := mkdirSafe(o.originals, dir); e != nil {
		return e
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	for _, d := range entries {
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, d.Name())
		if e = checkPath(o.originals, p, false); e != nil {
			return e
		}
		data, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		var j operation
		if e = json.Unmarshal(data, &j); e != nil {
			return fmt.Errorf("invalid journal %s: %w", p, e)
		}
		if o.db == nil {
			return fmt.Errorf("unresolved journal %s: database required", p)
		}
		var folder string
		if e = o.db.QueryRowContext(ctx, `SELECT folder_path FROM books WHERE id=$1 AND library_id=$2`, j.Survivor, j.Library).Scan(&folder); e != nil {
			return fmt.Errorf("unresolved journal %s: %w", p, e)
		}
		old, new := folder == j.OldFolder, folder == j.NewFolder
		for _, m := range j.Moves {
			var path string
			var owner int
			if e = o.db.QueryRowContext(ctx, `SELECT absolute_path,book_id FROM book_files WHERE id=$1`, m.ID).Scan(&path, &owner); e != nil {
				return fmt.Errorf("unresolved journal %s: %w", p, e)
			}
			old = old && path == m.From && owner == m.OldBook
			new = new && path == m.To && owner == j.Survivor
		}
		if old == new {
			return fmt.Errorf("unresolved ambiguous journal %s", p)
		}
		for _, m := range j.Moves {
			if m.From == m.To {
				continue
			}
			if e = checkPath(o.root, m.From, false); e != nil {
				return e
			}
			if e = checkPath(o.root, m.To, false); e != nil {
				return e
			}
			keep, drop := m.From, m.To
			if new {
				keep, drop = m.To, m.From
			}
			h, e := hashFile(keep)
			if errors.Is(e, os.ErrNotExist) {
				other, readErr := hashFile(drop)
				if readErr != nil || other != m.Hash {
					return fmt.Errorf("unresolved journal %s: neither valid file available", p)
				}
				if e = claimFile(drop, keep); e != nil {
					return e
				}
				if e = syncDir(filepath.Dir(keep)); e != nil {
					return e
				}
			} else if e != nil || h != m.Hash {
				return fmt.Errorf("unresolved journal %s: changed retained file %s", p, keep)
			}
			h, e = hashFile(drop)
			if e == nil {
				if h != m.Hash {
					return fmt.Errorf("unresolved journal %s: changed file %s", p, drop)
				}
				if e = os.Remove(drop); e != nil {
					return e
				}
				if e = syncDir(filepath.Dir(drop)); e != nil {
					return e
				}
			} else if !errors.Is(e, os.ErrNotExist) {
				return e
			}
		}
		if e = os.Remove(p); e != nil {
			return e
		}
		if e = syncDir(dir); e != nil {
			return e
		}
	}
	return nil
}

// Organize only moves this record. Merging waits until all saved curation has
// been applied and a reconciliation plan has selected the survivor's metadata.
func (o *Organizer) Organize(ctx context.Context, book domain.Book) (Result, error) {
	return o.OrganizeGroup(ctx, book, []int{book.ID})
}

func (o *Organizer) OrganizeGroup(ctx context.Context, book domain.Book, ids []int) (result Result, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.db == nil {
		return result, errors.New("organization disabled: no PostgreSQL DSN")
	}
	if err = o.recover(ctx); err != nil {
		return
	}
	tx, err := o.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	// Lock the library row to serialize participating organizers; scans must also be disabled externally.
	var library int
	err = tx.QueryRowContext(ctx, `SELECT library_id FROM books WHERE id=$1`, book.ID).Scan(&library)
	if err != nil {
		return
	}
	if book.LibraryID != 0 && book.LibraryID != library {
		return result, errors.New("library ID changed")
	}
	if err = tx.QueryRowContext(ctx, `SELECT id FROM libraries WHERE id=$1 FOR UPDATE`, library).Scan(&library); err != nil {
		return
	}
	var running bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM scan_jobs WHERE library_id=$1 AND status='running')`, library).Scan(&running); err != nil {
		return
	}
	if running {
		return result, errors.New("library scan is running")
	}
	records, err := o.records(ctx, tx, library)
	if err != nil {
		return
	}
	idx := -1
	for i := range records {
		if records[i].book.ID == book.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return result, errors.New("book disappeared")
	}
	base := records[idx]
	selected := []record{base}
	for _, id := range ids {
		if id == base.book.ID {
			continue
		}
		found := false
		for _, r := range records {
			if r.book.ID != id {
				continue
			}
			if r.folderID != base.folderID {
				return result, errors.New("merge crosses library folder")
			}
			selected = append(selected, r)
			found = true
		}
		if !found {
			return result, fmt.Errorf("merge member %d disappeared", id)
		}
	}
	// Snapshot all references and transfer personal data before moving any bytes.
	// A new/unsupported schema or conflicting unique key fails the transaction.
	for _, r := range selected[1:] {
		if err = o.transferReferences(ctx, tx, r.book.ID, base.book.ID); err != nil {
			return
		}
	}

	if err = checkPath(o.root, base.folderRoot, true); err != nil {
		return
	}
	if !within(o.root, base.folderRoot) {
		return result, errors.New("library folder outside root")
	}
	for i := range selected {
		if err = files(ctx, tx, &selected[i]); err != nil {
			return
		}
		if len(selected[i].book.Files) == 0 {
			return result, fmt.Errorf("book %d has no files", selected[i].book.ID)
		}
		if err = o.backup(ctx, selected[i].book); err != nil {
			return
		}
	}
	title := component(base.book.Metadata.Title, "Untitled")
	author := "Unknown Author"
	if len(base.book.Metadata.Authors) > 0 {
		author = component(base.book.Metadata.Authors[0], author)
	}
	parts := []string{base.folderRoot, author}
	if base.book.Metadata.SeriesName != "" {
		parts = append(parts, component(base.book.Metadata.SeriesName, "Unknown Series"))
	}
	parts = append(parts, fmt.Sprintf("%s [book-%d]", title, base.book.ID))
	folder := filepath.Join(parts...)
	if err = checkPath(o.root, folder, false); err != nil {
		return
	}
	if !within(base.folderRoot, folder) || folder == base.folderRoot {
		return result, errors.New("destination outside library folder")
	}
	if err = mkdirSafe(o.root, folder); err != nil {
		return
	}
	j := operation{Library: library, Survivor: base.book.ID, OldFolder: base.folder, NewFolder: folder}
	reserved := map[string]bool{}
	for _, r := range selected {
		if r.book.ID != base.book.ID {
			result.MergedBookIDs = append(result.MergedBookIDs, r.book.ID)
		}
		for _, f := range r.book.Files {
			if err = checkPath(o.root, f.AbsolutePath, false); err != nil {
				return
			}
			if !within(base.folderRoot, f.AbsolutePath) {
				return result, errors.New("file outside library folder")
			}
			var h string
			h, err = hashFile(f.AbsolutePath)
			if err != nil {
				return
			}
			ext := filepath.Ext(f.AbsolutePath)
			name := title + ext
			if f.Role != "content" {
				name = component(strings.TrimSuffix(filepath.Base(f.AbsolutePath), ext), "file") + ext
			}
			dst := filepath.Join(folder, name)
			if reserved[dst] || dst != f.AbsolutePath && pathExists(dst) {
				dst = filepath.Join(folder, fmt.Sprintf("%s [file-%d]%s", strings.TrimSuffix(name, ext), f.ID, ext))
			}
			if reserved[dst] || dst != f.AbsolutePath && pathExists(dst) {
				return result, fmt.Errorf("destination already exists: %s", dst)
			}
			if err = checkPath(o.root, dst, false); err != nil {
				return
			}
			reserved[dst] = true
			j.Moves = append(j.Moves, entry{move: move{ID: f.ID, From: f.AbsolutePath, To: dst, Hash: h}, OldBook: r.book.ID})
		}
	}
	changed := base.folder != folder || len(result.MergedBookIDs) > 0
	for _, m := range j.Moves {
		changed = changed || m.From != m.To
	}
	if !changed {
		return result, nil
	}
	dir := filepath.Join(o.originals, "journals")
	if err = mkdirSafe(o.originals, dir); err != nil {
		return
	}
	journalPath := filepath.Join(dir, fmt.Sprintf("%d-%d.json", library, base.book.ID))
	if err = checkPath(o.originals, journalPath, false); err != nil {
		return
	}
	if err = writeJSON(journalPath, j); err != nil {
		return
	}
	// Roll back before consulting committed paths; a failed commit may have succeeded.
	defer func() {
		if err != nil {
			_ = tx.Rollback()
			if recoveryErr := o.recover(context.Background()); recoveryErr != nil {
				err = errors.Join(err, fmt.Errorf("journal recovery: %w", recoveryErr))
			}
		}
	}()
	for _, m := range j.Moves {
		if m.From == m.To {
			continue
		}
		if err = ctx.Err(); err != nil {
			return
		}
		if err = moveExclusive(m.From, m.To); err != nil {
			return
		}
		result.MovedFileCount++
	}
	for _, m := range j.Moves {
		rel, e := filepath.Rel(base.folderRoot, m.To)
		if e != nil {
			return result, e
		}
		info, e := os.Stat(m.To)
		if e != nil {
			return result, e
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return result, errors.New("filesystem inode unavailable")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE book_files SET book_id=$1,absolute_path=$2,rel_path=$3,ino=$4,updated_at=now() WHERE id=$5`, base.book.ID, m.To, filepath.ToSlash(rel), strconv.FormatUint(stat.Ino, 10), m.ID); err != nil {
			return
		}
	}
	for _, id := range result.MergedBookIDs {
		if _, err = tx.ExecContext(ctx, `DELETE FROM books WHERE id=$1`, id); err != nil {
			return
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE books SET folder_path=$1,primary_file_id=COALESCE(primary_file_id,(SELECT id FROM book_files WHERE book_id=$2 ORDER BY CASE WHEN role='content' THEN 0 ELSE 1 END,id LIMIT 1)),updated_at=now() WHERE id=$2`, folder, base.book.ID); err != nil {
		return
	}
	if err = tx.Commit(); err != nil {
		return
	}
	if err = os.Remove(journalPath); err != nil {
		return
	}
	err = syncDir(dir)
	return
}

func pathExists(p string) bool { _, e := os.Lstat(p); return e == nil || !errors.Is(e, os.ErrNotExist) }

// moveExclusive claims a destination atomically and never replaces an existing file.
func moveExclusive(from, to string) error {
	if err := claimFile(from, to); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(to)); err != nil {
		return err
	}
	if err := os.Remove(from); err != nil {
		return err
	}
	return syncDir(filepath.Dir(from))
}

func claimFile(from, to string) error {
	err := os.Link(from, to)
	if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) {
		return err
	}
	// Linux protected_hardlinks rejects readable files owned by another NAS
	// identity. Copy into a private temporary file, verify the bytes against the
	// still-present source, then atomically claim the destination with that link.
	return copyClaimFile(from, to)
}

func copyClaimFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("move source is not a regular file")
	}
	tmp, err := os.CreateTemp(filepath.Dir(to), ".librarian-move-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	defer func() { _ = tmp.Close() }()
	h := sha256.New()
	if _, err = io.Copy(io.MultiWriter(tmp, h), in); err != nil {
		return err
	}
	if err = tmp.Chmod(st.Mode().Perm()); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	current, err := hashFile(from)
	if err != nil {
		return err
	}
	if current != hex.EncodeToString(h.Sum(nil)) {
		return errors.New("move source changed while copying")
	}
	if err = os.Chtimes(name, st.ModTime(), st.ModTime()); err != nil {
		return err
	}
	return os.Link(name, to)
}
