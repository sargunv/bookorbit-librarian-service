package organize

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

type Reference struct {
	Table  string            `json:"table"`
	Column string            `json:"column"`
	Rows   []json.RawMessage `json:"rows"`
}

type Snapshot struct {
	References []Reference `json:"references"`
}

func quoted(name string) string { return pgx.Identifier{name}.Sanitize() }

func references(ctx context.Context, tx *sql.Tx) ([]Reference, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT rel.relname,a.attname FROM pg_constraint c JOIN pg_class rel ON rel.oid=c.conrelid JOIN pg_namespace ns ON ns.oid=rel.relnamespace CROSS JOIN LATERAL unnest(c.conkey,c.confkey) AS keys(local_num,target_num) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=keys.local_num JOIN pg_attribute target ON target.attrelid=c.confrelid AND target.attnum=keys.target_num WHERE c.contype='f' AND c.confrelid='public.books'::regclass AND target.attname='id' AND ns.nspname='public' ORDER BY rel.relname,a.attname`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var refs []Reference
	for rows.Next() {
		var r Reference
		if err = rows.Scan(&r.Table, &r.Column); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// These are the curated record itself or derived queues/caches. Their original
// rows are archived, while the survivor's API-curated metadata remains canonical.
// Everything else must be transferred successfully, including future FK tables.
func curatedOrDerived(table string) bool {
	return slices.Contains([]string{"book_metadata", "book_authors", "book_genres", "book_provider_ids", "book_series_memberships", "book_covers", "book_metadata_fetch_queue", "chapter_lookup_queue", "book_chapter_jobs", "book_duplicate_dismissals", "book_duplicate_group_members", "book_duplicate_groups", "book_duplicate_pairs", "book_duplicate_scan_keys"}, table)
}

func (o *Organizer) transferReferences(ctx context.Context, tx *sql.Tx, old, dest int) error {
	refs, err := references(ctx, tx)
	if err != nil {
		return err
	}
	for i := range refs {
		r := &refs[i]
		lock := " FOR UPDATE"
		if curatedOrDerived(r.Table) {
			lock = ""
		}
		// Read-only bibliographic tables need no UPDATE grant merely to archive
		// their rows. Parent books are already locked by the organizer; mutable
		// personal references retain row locks through transfer and deletion.
		rows, e := tx.QueryContext(ctx, "SELECT to_jsonb(t)::text FROM public."+quoted(r.Table)+" t WHERE "+quoted(r.Column)+" IN ($1,$2)"+lock, old, dest)
		if e != nil {
			return fmt.Errorf("read merge references %s: %w", r.Table, e)
		}
		for rows.Next() {
			var raw string
			if e = rows.Scan(&raw); e != nil {
				_ = rows.Close()
				return e
			}
			r.Rows = append(r.Rows, json.RawMessage(raw))
		}
		e = rows.Err()
		_ = rows.Close()
		if e != nil {
			return e
		}
	}
	dir := filepath.Join(o.originals, "merges")
	if err = mkdirSafe(o.originals, dir); err != nil {
		return err
	}
	// Immutable attempts also retain both versions of unique-key conflicts.
	data, err := json.Marshal(Snapshot{References: refs})
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	path := filepath.Join(dir, fmt.Sprintf("%d-into-%d-%x.json", old, dest, hash[:8]))
	if !pathExists(path) {
		if err = writeJSON(path, Snapshot{References: refs}); err != nil {
			return err
		}
	}
	for _, r := range refs {
		if r.Table == "book_metadata" && len(r.Rows) > 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE book_metadata SET locked_fields=ARRAY(SELECT DISTINCT unnest(coalesce(locked_fields,'{}') || coalesce((SELECT locked_fields FROM book_metadata WHERE book_id=$2),'{}'))) WHERE book_id=$1`, dest, old); err != nil {
				return err
			}
		}
		if len(r.Rows) == 0 || r.Table == "book_files" || curatedOrDerived(r.Table) {
			continue
		}
		if err = transfer(ctx, tx, r, old, dest, false); err != nil {
			return fmt.Errorf("preserve %s before merging book %d: %w", r.Table, old, err)
		}
	}
	return nil
}

func primaryKey(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.attname FROM pg_index i CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(num,ord) JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.num WHERE i.indrelid=to_regclass($1) AND i.indisprimary ORDER BY k.ord`, "public."+quoted(table))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func transfer(ctx context.Context, tx *sql.Tx, r Reference, old, dest int, restore bool) error {
	keys, err := primaryKey(ctx, tx, r.Table)
	if err != nil {
		return err
	}
	table, column := "public."+quoted(r.Table), quoted(r.Column)
	if !slices.Contains(keys, r.Column) && !restore {
		// Preserve surrogate IDs and their dependent children. Any other UNIQUE
		// conflict is an error and rolls back before files move or a book is deleted.
		_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET "+column+"=$1 WHERE "+column+"=$2", dest, old)
		return err
	}
	for _, raw := range r.Rows {
		var row map[string]json.RawMessage
		if err = json.Unmarshal(raw, &row); err != nil {
			return err
		}
		row[r.Column], _ = json.Marshal(dest)
		data, _ := json.Marshal(row)
		conflict := ""
		if restore && !slices.Contains(keys, r.Column) && len(keys) > 0 {
			// A previously restored surrogate ID must still belong to the same
			// survivor. Keep later user edits on retry, never silently accept reuse.
			var conditions []string
			var args []any
			for i, k := range keys {
				conditions = append(conditions, "to_jsonb(target)->"+"'"+strings.ReplaceAll(k, "'", "''")+"'=$"+fmt.Sprint(i+1)+"::jsonb")
				args = append(args, string(row[k]))
			}
			var owner sql.NullInt64
			e := tx.QueryRowContext(ctx, "SELECT "+column+" FROM "+table+" target WHERE "+strings.Join(conditions, " AND "), args...).Scan(&owner)
			if e == nil {
				if owner.Valid && owner.Int64 != int64(dest) {
					return errors.New("restored primary key belongs to another book")
				}
				if !owner.Valid {
					args = append(args, dest)
					if _, err = tx.ExecContext(ctx, "UPDATE "+table+" AS target SET "+column+"=$"+fmt.Sprint(len(args))+" WHERE "+strings.Join(conditions, " AND "), args...); err != nil {
						return err
					}
				}
				continue
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		}
		if slices.Contains(keys, r.Column) {
			qkeys := make([]string, len(keys))
			for i, k := range keys {
				qkeys[i] = quoted(k)
			}
			conflict = " ON CONFLICT (" + strings.Join(qkeys, ",") + ") DO NOTHING"
			// A per-user conflict keeps the latest value. All source versions are
			// retained in the private snapshot. Equal timestamps prefer a completed
			// reading status over an untouched to-read/not-started entry.
			if _, ok := row["updated_at"]; ok {
				var sets []string
				var fields []string
				for field := range row {
					fields = append(fields, field)
				}
				slices.Sort(fields)
				for _, field := range fields {
					if !slices.Contains(keys, field) {
						sets = append(sets, quoted(field)+"=EXCLUDED."+quoted(field))
					}
				}
				newer := "EXCLUDED.updated_at > COALESCE(target.updated_at,'-infinity'::timestamptz)"
				if r.Table == "user_book_status" {
					rank := func(alias string) string {
						return "CASE lower(" + alias + ".status) WHEN 'read' THEN 4 WHEN 'finished' THEN 4 WHEN 'reading' THEN 3 WHEN 'paused' THEN 2 ELSE 1 END"
					}
					newer += " OR (EXCLUDED.updated_at = target.updated_at AND " + rank("EXCLUDED") + ">" + rank("target") + ")"
				}
				if len(sets) > 0 {
					conflict = " ON CONFLICT (" + strings.Join(qkeys, ",") + ") DO UPDATE SET " + strings.Join(sets, ",") + " WHERE " + newer
				}
			}
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO "+table+" AS target OVERRIDING SYSTEM VALUE SELECT (jsonb_populate_record(NULL::"+table+",$1::jsonb)).*"+conflict, string(data))
		if err != nil {
			return err
		}
	}
	return nil
}

// RestoreReferences imports only books with a recorded old->survivor alias.
// It never restores stale bibliographic metadata or changes a surviving book ID.
type RecoveryReport struct {
	Applied            int
	RetiredMemberships int
}

func (o *Organizer) RestoreReferences(ctx context.Context, snapshot Snapshot, aliases map[int]int) (RecoveryReport, error) {
	var report RecoveryReport
	if o.db == nil {
		return report, errors.New("reference recovery requires PostgreSQL")
	}
	tx, err := o.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return report, err
	}
	defer func() { _ = tx.Rollback() }()
	allowed, err := references(ctx, tx)
	if err != nil {
		return report, err
	}
	for _, ref := range snapshot.References {
		if curatedOrDerived(ref.Table) || ref.Table == "book_files" {
			continue
		}
		if !slices.ContainsFunc(allowed, func(r Reference) bool { return r.Table == ref.Table && r.Column == ref.Column }) {
			return report, fmt.Errorf("snapshot has unknown book reference %s.%s", ref.Table, ref.Column)
		}
		for _, raw := range ref.Rows {
			var row map[string]json.RawMessage
			if err = json.Unmarshal(raw, &row); err != nil {
				return report, err
			}
			var old int
			if err = json.Unmarshal(row[ref.Column], &old); err != nil {
				return report, err
			}
			dest := aliases[old]
			if dest == 0 {
				continue
			}
			for _, other := range allowed {
				if other.Table != ref.Table {
					continue
				}
				var id int
				if json.Unmarshal(row[other.Column], &id) == nil && aliases[id] != 0 {
					row[other.Column], _ = json.Marshal(aliases[id])
				}
			}
			raw, _ = json.Marshal(row)
			if ref.Table == "collection_books" {
				var collection int
				if err = json.Unmarshal(row["collection_id"], &collection); err != nil {
					return report, err
				}
				var exists bool
				if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM collections WHERE id=$1)", collection).Scan(&exists); err != nil {
					return report, err
				}
				if !exists {
					report.RetiredMemberships++
					continue
				}
			}
			// Restoring before new merges uses the historical live target. New
			// group aliases are applied by the normal lossless merge afterwards.
			one := Reference{Table: ref.Table, Column: ref.Column, Rows: []json.RawMessage{raw}}
			if err = transfer(ctx, tx, one, old, dest, true); err != nil {
				return report, fmt.Errorf("restore %s for book %d: %w", ref.Table, old, err)
			}
			report.Applied++
		}
	}
	return report, tx.Commit()
}

func (o *Organizer) Inventory(ctx context.Context, library int) ([]domain.Book, error) {
	if o.db == nil {
		return nil, errors.New("inventory requires PostgreSQL")
	}
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	records, err := o.records(ctx, tx, library)
	if err != nil {
		return nil, err
	}
	books := make([]domain.Book, 0, len(records))
	for i := range records {
		if err = files(ctx, tx, &records[i]); err != nil {
			return nil, err
		}
		books = append(books, records[i].book)
	}
	return books, tx.Commit()
}
