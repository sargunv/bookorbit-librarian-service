package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sargunv/bookorbit-librarian-service/internal/curator"
	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
	"github.com/sargunv/bookorbit-librarian-service/internal/organize"
	"github.com/sargunv/bookorbit-librarian-service/internal/reconcile"
)

type ReconcileReport struct {
	Groups             int `json:"verifiedGroups"`
	Merged             int `json:"mergedBooks"`
	Restored           int `json:"recoveredReferenceRows"`
	RetiredMemberships int `json:"retiredCollectionMemberships"`
}

func (s *Service) reconciliationPlan(ctx context.Context) (reconcile.Plan, error) {
	books, err := s.organizer.Inventory(ctx, s.cfg.LibraryID)
	if err != nil {
		return reconcile.Plan{}, err
	}
	jobs, err := s.db.AllJobs(ctx)
	if err != nil {
		return reconcile.Plan{}, err
	}
	aliases, err := s.db.Aliases(ctx)
	if err != nil {
		return reconcile.Plan{}, err
	}
	plan, err := reconcile.Build(books, jobs, aliases)
	if err != nil {
		return plan, err
	}
	var groups []reconcile.Group
	for _, g := range plan.Groups {
		if g.CoverURL != "" {
			done, e := s.db.ReconciledCover(ctx, g.Survivor, g.CoverURL)
			if e != nil {
				return plan, e
			}
			if done {
				g.CoverURL = ""
				g.CoverSource = ""
			}
		}
		if len(g.Patch) > 0 || len(g.BookIDs) > 1 || g.CoverURL != "" {
			groups = append(groups, g)
		}
	}
	plan.Groups = groups
	return plan, nil
}

// ReconcileFile prepares or resumes a private durable plan. No provider/model
// calls occur here. Run with the regular service stopped, or through the idle
// worker's serialized final pass. Recovery snapshots are operator-supplied.
func (s *Service) ReconcileFile(ctx context.Context, path string, apply bool, recoveryPath string) (ReconcileReport, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	var report ReconcileReport
	var plan reconcile.Plan
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		plan, err = s.reconciliationPlan(ctx)
		if err != nil {
			return report, err
		}
		raw, err = json.MarshalIndent(plan, "", "  ")
		if err == nil {
			err = privateFile(path, raw)
		}
	} else if err == nil {
		err = json.Unmarshal(raw, &plan)
	}
	if err != nil {
		return report, err
	}
	if plan.Version != 1 {
		return report, errors.New("unsupported reconciliation plan version")
	}
	if !apply {
		report.Groups = len(plan.Groups)
		for _, g := range plan.Groups {
			report.Merged += len(g.BookIDs) - 1
		}
		return report, nil
	}
	if recoveryPath != "" {
		data, err := os.ReadFile(recoveryPath)
		if err != nil {
			return report, err
		}
		var snapshot organize.Snapshot
		if err = json.Unmarshal(data, &snapshot); err != nil {
			return report, err
		}
		jobs, err := s.db.AllJobs(ctx)
		if err != nil {
			return report, err
		}
		aliases, err := reconcile.HistoricalAliases(jobs)
		if err != nil {
			return report, err
		}
		// On a resumed repair a former survivor may itself have been merged.
		for old := range aliases {
			if dest, e := s.db.ResolveBook(ctx, aliases[old]); e == nil {
				aliases[old] = dest
			} else {
				return report, e
			}
		}
		recovery, restoreErr := s.organizer.RestoreReferences(ctx, snapshot, aliases)
		report.Restored, report.RetiredMemberships = recovery.Applied, recovery.RetiredMemberships
		err = restoreErr
		if err != nil {
			return report, err
		}
	}
	hash := sha256.Sum256(raw)
	run := hex.EncodeToString(hash[:])
	for _, group := range plan.Groups {
		if err = s.applyReconciliation(ctx, run, plan.Aliases, group); err != nil {
			return report, fmt.Errorf("reconcile book %d: %w", group.Survivor, err)
		}
		report.Groups++
		report.Merged += len(group.BookIDs) - 1
	}
	// Persist historical aliases even when no book needed another metadata edit.
	if err = s.db.Reconciled(ctx, run, plan.Aliases, domain.ActivityEvent{}, ""); err != nil {
		return report, err
	}
	return report, nil
}

func (s *Service) applyReconciliation(ctx context.Context, run string, aliases map[int]int, g reconcile.Group) error {
	before, err := s.books.Book(ctx, g.Survivor)
	if err != nil {
		return err
	}
	current := domain.Values(before)
	done, err := s.db.ReconciliationDone(ctx, run, g.Survivor)
	if err != nil {
		return err
	}
	patch := domain.Patch{}
	var changes []domain.FieldChange
	for field, value := range g.Patch {
		if domain.EqualField(field, value, current[field]) {
			continue
		}
		if len(current[field]) > 0 && !domain.EqualField(field, current[field], g.Before[field]) {
			return fmt.Errorf("field %s changed since this plan was saved", field)
		}
		if slices.Contains(before.LockedFields, field) {
			return fmt.Errorf("field %s became locked", field)
		}
		patch[field] = value
		old := current[field]
		if len(old) == 0 {
			old = json.RawMessage("null")
		}
		changes = append(changes, domain.FieldChange{Field: field, Before: old, After: value, Source: g.Sources[field]})
	}
	if err = s.organizer.Backup(ctx, before); err != nil {
		return err
	}
	if len(patch) > 0 {
		if err = s.books.Apply(ctx, g.Survivor, patch); err != nil {
			return err
		}
	}
	if g.CoverURL != "" && !done {
		if slices.Contains(before.LockedFields, "cover") {
			return errors.New("cover became locked")
		}
		if err = s.books.Cover(ctx, g.Survivor, g.CoverURL); err != nil {
			return err
		}
		if _, err = s.books.CoverImage(ctx, g.Survivor); err != nil {
			return err
		}
	}
	fresh, err := s.books.Book(ctx, g.Survivor)
	if err != nil {
		return err
	}
	// The saved group is authoritative. Missing siblings are accepted only when
	// all their exact file IDs already belong to the survivor (interrupted retry).
	var live []int
	for _, id := range g.BookIDs {
		if id == g.Survivor {
			live = append(live, id)
			continue
		}
		_, e := s.books.Book(ctx, id)
		if e == nil {
			live = append(live, id)
		} else if !strings.Contains(e.Error(), "HTTP 404") {
			return e
		}
	}
	result, err := s.organizer.OrganizeGroup(ctx, fresh, live)
	if err != nil {
		return err
	}
	if len(patch) > 0 || len(g.BookIDs) > 1 || g.CoverURL != "" && !done {
		if err = s.books.Write(ctx, g.Survivor); err != nil {
			return err
		}
	}
	after, err := s.books.Book(ctx, g.Survivor)
	if err != nil {
		return err
	}
	if err = verifyPatch(after, g.Patch); err != nil {
		return err
	}
	for _, id := range g.FileIDs {
		if !slices.ContainsFunc(after.Files, func(f domain.File) bool { return f.ID == id }) {
			return fmt.Errorf("survivor is missing file %d", id)
		}
	}
	for _, id := range g.BookIDs {
		if id != g.Survivor {
			_, e := s.books.Book(ctx, id)
			if e == nil || !strings.Contains(e.Error(), "HTTP 404") {
				return fmt.Errorf("merged book %d still exists or could not be verified", id)
			}
		}
	}
	coverage := domain.MetadataCoverage(before.Metadata, g.Patch)
	event := domain.ActivityEvent{JobID: g.JobID, BookID: g.Survivor, Title: after.Metadata.Title, Policy: curator.PolicyVersion, Kind: "applied", Message: "Reconciled saved curation and verified surviving record", Details: domain.ActivityDetails{Changes: changes, Coverage: &coverage, CoverChanged: g.CoverURL != "", CoverSource: g.CoverSource, MergedBooks: result.MergedBookIDs, MovedFiles: result.MovedFileCount, Reason: "Reused saved source selections; transferred personal data before deleting merged records"}}
	return s.db.Reconciled(ctx, run, aliasesForGroup(aliases, g.Survivor), event, g.CoverURL)
}

func aliasesForGroup(aliases map[int]int, survivor int) map[int]int {
	out := map[int]int{}
	for old, dest := range aliases {
		if dest == survivor {
			out[old] = dest
		}
	}
	return out
}

func verifyPatch(book domain.Book, patch domain.Patch) error {
	values := domain.Values(book)
	for field, want := range patch {
		if !domain.EqualField(field, want, values[field]) {
			return fmt.Errorf("BookOrbit did not retain field %s", field)
		}
	}
	return nil
}

func privateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".plan-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
