package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/sargunv/bookorbit-librarian-service/internal/curator"
	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

func groupFor(plan *domain.Discovery, id int) *domain.Group {
	if plan == nil {
		return nil
	}
	for i := range plan.Groups {
		for _, member := range plan.Groups[i].BookIDs {
			if member == id {
				return &plan.Groups[i]
			}
		}
	}
	return nil
}

// discoverGroups caches an inventory-wide grouping snapshot. Editing metadata
// does not continuously invalidate it; new books or an explicit refresh do.
func (s *Service) discoverGroups(ctx context.Context, ids []int, force bool) (*domain.Discovery, error) {
	s.discoveryMu.Lock()
	defer s.discoveryMu.Unlock()
	if s.discovery == nil {
		saved, err := s.db.Discovery(ctx, curator.PolicyVersion)
		if err == nil {
			s.discovery = &saved
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	complete := s.discovery != nil
	for _, id := range ids {
		if groupFor(s.discovery, id) == nil {
			complete = false
			break
		}
	}
	if complete && !force {
		return s.discovery, nil
	}
	bounded, cancel := context.WithTimeout(ctx, s.cfg.JobTimeout)
	defer cancel()
	books := make([]domain.Book, 0, len(ids))
	for _, id := range ids {
		book, err := s.books.Book(bounded, id)
		if err != nil {
			if strings.Contains(err.Error(), "HTTP 404") {
				continue
			}
			return nil, err
		}
		books = append(books, book)
	}
	plan, err := s.judge.Discover(bounded, books)
	if err != nil {
		return nil, err
	}
	if err = s.db.SaveDiscovery(bounded, curator.PolicyVersion, plan); err != nil {
		return nil, err
	}
	s.discovery = &plan
	return s.discovery, nil
}

func (s *Service) groupMembers(ctx context.Context, current domain.Book) ([]domain.Book, error) {
	s.discoveryMu.Lock()
	if s.discovery == nil {
		saved, err := s.db.Discovery(ctx, curator.PolicyVersion)
		if err == nil {
			s.discovery = &saved
		} else if !errors.Is(err, sql.ErrNoRows) {
			s.discoveryMu.Unlock()
			return nil, err
		}
	}
	group := groupFor(s.discovery, current.ID)
	s.discoveryMu.Unlock()
	if group == nil {
		ids, err := s.books.ListBooks(ctx, s.cfg.LibraryID)
		if err != nil {
			return nil, err
		}
		plan, err := s.discoverGroups(ctx, ids, false)
		if err != nil {
			return nil, err
		}
		group = groupFor(plan, current.ID)
	}
	if group == nil {
		return nil, errors.New("book absent from library discovery")
	}
	members := make([]domain.Book, 0, len(group.BookIDs))
	for _, id := range group.BookIDs {
		if id == current.ID {
			members = append(members, current)
			continue
		}
		book, err := s.books.Book(ctx, id)
		if err != nil {
			if strings.Contains(err.Error(), "HTTP 404") {
				continue
			}
			return nil, err
		}
		if book.LibraryID == s.cfg.LibraryID {
			members = append(members, book)
		}
	}
	return members, nil
}
