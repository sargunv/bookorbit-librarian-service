// Package bookorbit implements the BookOrbit v3 API adapter.
package bookorbit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

const maxBody = 16 << 20

type Client struct {
	base  string
	magic string
	http  *http.Client
	mu    sync.Mutex
	token string
}

func New(baseURL, magicLinkToken string) *Client {
	base := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(base, "/api/v1") {
		base += "/api/v1"
	}
	return &Client{base: base, magic: magicLinkToken, http: &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// authenticate serializes refreshes and avoids renewing a token another caller already replaced.
func (c *Client) authenticate(ctx context.Context, stale string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && (stale == "" || c.token != stale) {
		return c.token, nil
	}
	payload, _ := json.Marshal(map[string]string{"token": c.magic})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/auth/magic-links/login", bytes.NewReader(payload))
	if err != nil {
		return "", errors.New("bookorbit: invalid authentication URL")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("bookorbit: authentication request failed: %w", safeTransport(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("bookorbit: authentication HTTP %d", resp.StatusCode)
	}
	var data struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&data); err != nil || data.AccessToken == "" {
		return "", errors.New("bookorbit: invalid authentication response")
	}
	c.token = data.AccessToken
	return c.token, nil
}

// Transport errors can contain URLs and query parameters. Keep only the underlying cause.
func safeTransport(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

func (c *Client) request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	token, err := c.authenticate(ctx, "")
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, e := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
		if e != nil {
			return nil, errors.New("bookorbit: invalid request URL")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, e := c.http.Do(req)
		if e != nil {
			return nil, fmt.Errorf("bookorbit: request failed: %w", safeTransport(e))
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_ = resp.Body.Close()
			token, err = c.authenticate(ctx, token)
			if err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("bookorbit: HTTP %d", resp.StatusCode)
		}
		return resp, nil
	}
	return nil, errors.New("bookorbit: unauthorized")
}

func (c *Client) json(ctx context.Context, method, path string, in, out interface{}) error {
	var b []byte
	var err error
	if in != nil {
		b, err = json.Marshal(in)
		if err != nil {
			return fmt.Errorf("bookorbit: encode request: %w", err)
		}
	}
	resp, err := c.request(ctx, method, path, b)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("bookorbit: read response: %w", err)
	}
	if len(data) > maxBody {
		return errors.New("bookorbit: response exceeds size limit")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("bookorbit: invalid response: %w", err)
	}
	return nil
}
func bookPath(id int) string { return "/books/" + strconv.Itoa(id) }

func (c *Client) Book(ctx context.Context, id int) (domain.Book, error) {
	var raw struct {
		domain.Metadata
		ID        int    `json:"id"`
		LibraryID int    `json:"libraryId"`
		UpdatedAt string `json:"updatedAt"`
		Authors   []struct {
			Name string `json:"name"`
		} `json:"authors"`
		Files              []domain.File     `json:"files"`
		ProviderIDs        map[string]string `json:"providerIds"`
		HardcoverEditionID string            `json:"hardcoverEditionId"`
		LockedFields       []string          `json:"lockedFields"`
	}
	err := c.json(ctx, http.MethodGet, bookPath(id), nil, &raw)
	if err != nil {
		return domain.Book{}, err
	}
	for _, a := range raw.Authors {
		raw.Metadata.Authors = append(raw.Metadata.Authors, a.Name)
	}
	return domain.Book{ID: raw.ID, LibraryID: raw.LibraryID, UpdatedAt: raw.UpdatedAt, Metadata: raw.Metadata, Files: raw.Files, ProviderIDs: raw.ProviderIDs, HardcoverEditionID: raw.HardcoverEditionID, LockedFields: raw.LockedFields}, nil
}

func (c *Client) ListBooks(ctx context.Context, libraryID int) ([]int, error) {
	// The global query filters library by name, not ID. Read each card's libraryId instead.
	var ids []int
	for page := 0; ; page++ {
		query := struct {
			Sort       []map[string]string `json:"sort"`
			Pagination map[string]int      `json:"pagination"`
		}{[]map[string]string{{"field": "title", "dir": "asc"}}, map[string]int{"page": page, "size": 200}}
		var result struct {
			Items []struct {
				ID        int  `json:"id"`
				LibraryID *int `json:"libraryId"`
			} `json:"items"`
			Total *int `json:"total"`
		}
		if err := c.json(ctx, http.MethodPost, "/books/query", query, &result); err != nil {
			return nil, err
		}
		if result.Total == nil {
			return nil, errors.New("bookorbit: query response missing total")
		}
		for _, item := range result.Items {
			if item.LibraryID == nil {
				return nil, errors.New("bookorbit: query response missing libraryId")
			}
			if *item.LibraryID == libraryID {
				ids = append(ids, item.ID)
			}
		}
		if (page+1)*200 >= *result.Total {
			return ids, nil
		}
		if len(result.Items) == 0 {
			return nil, errors.New("bookorbit: incomplete query pagination")
		}
	}
}

// SearchError reports provider interruptions while retaining any candidates returned by Search.
type SearchError struct{ Statuses map[string]string }

func (e *SearchError) Error() string {
	return fmt.Sprintf("bookorbit: metadata providers did not complete: %v", e.Statuses)
}

// Search retains linked edition evidence and also searches independently of old
// identifiers. BookOrbit otherwise short-circuits to existing provider IDs,
// which can pin a wrongly cataloged book to the wrong language or work.
func (c *Client) Search(ctx context.Context, book domain.Book, providers []string) ([]domain.Candidate, error) {
	candidates, err := c.search(ctx, book, providers, true)
	var partial *SearchError
	if err != nil && !errors.As(err, &partial) {
		return candidates, err
	}
	if book.Metadata.Title == "" {
		return candidates, err
	}
	fresh, freshErr := c.search(ctx, book, providers, false)
	type identity struct{ provider, id, edition, cover string }
	seen := map[identity]bool{}
	merged := make([]domain.Candidate, 0, len(candidates)+len(fresh))
	for _, candidate := range append(candidates, fresh...) {
		key := identity{candidate.Provider, candidate.ProviderID, candidate.HardcoverEditionID, candidate.CoverURL}
		if !seen[key] {
			merged = append(merged, candidate)
			seen[key] = true
		}
	}
	statuses := map[string]string{}
	if partial != nil {
		for k, v := range partial.Statuses {
			statuses[k] = v
		}
	}
	if freshErr != nil {
		var interruption *SearchError
		if errors.As(freshErr, &interruption) {
			for k, v := range interruption.Statuses {
				statuses["title-author/"+k] = v
			}
		} else if len(merged) == 0 {
			return nil, freshErr
		} else {
			statuses["title-author"] = "request failed"
		}
	}
	if len(statuses) > 0 {
		return merged, &SearchError{Statuses: statuses}
	}
	return merged, nil
}

func (c *Client) search(ctx context.Context, book domain.Book, providers []string, linked bool) ([]domain.Candidate, error) {
	if len(providers) == 0 {
		return nil, errors.New("bookorbit: at least one metadata provider is required")
	}
	q := url.Values{"providers": {strings.Join(providers, ",")}, "resultsPerProvider": {"5"}}
	if linked {
		q.Set("bookId", strconv.Itoa(book.ID))
	}
	if book.Metadata.Title != "" {
		q.Set("title", book.Metadata.Title)
	}
	if len(book.Metadata.Authors) > 0 {
		q.Set("author", strings.Join(book.Metadata.Authors, ", "))
	}
	isbn := book.Metadata.ISBN13
	if isbn == "" {
		isbn = book.Metadata.ISBN10
	}
	if linked && isbn != "" {
		q.Set("isbn", isbn)
	}
	resp, err := c.request(ctx, http.MethodGet, "/metadata-fetch/stream?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	limited := &io.LimitedReader{R: resp.Body, N: maxBody + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), maxBody)
	var candidates []domain.Candidate
	statuses := map[string]string{}
	var event string
	var data []string
	dispatch := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		payload := []byte(strings.Join(data, "\n"))
		data = nil
		name := event
		event = ""
		if name == "provider-status" {
			var s struct {
				Provider string `json:"provider"`
				Outcome  string `json:"outcome"`
			}
			if json.Unmarshal(payload, &s) != nil || s.Provider == "" || s.Outcome == "" {
				return errors.New("bookorbit: invalid provider status event")
			}
			statuses[s.Provider] = s.Outcome
			return nil
		}
		if name != "" && name != "message" {
			return fmt.Errorf("bookorbit: unexpected metadata event %q", name)
		}
		var candidate domain.Candidate
		if json.Unmarshal(payload, &candidate) != nil || candidate.Provider == "" {
			return errors.New("bookorbit: invalid candidate event")
		}
		candidates = append(candidates, candidate)
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return candidates, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			v = ""
		}
		v = strings.TrimPrefix(v, " ")
		switch k {
		case "event":
			event = v
		case "data":
			data = append(data, v)
		}
	}
	if err := scanner.Err(); err != nil {
		return candidates, fmt.Errorf("bookorbit: metadata stream: %w", err)
	}
	if limited.N == 0 {
		return candidates, errors.New("bookorbit: metadata stream exceeds size limit")
	}
	if err := dispatch(); err != nil {
		return candidates, err
	}
	if len(statuses) > 0 {
		return candidates, &SearchError{Statuses: statuses}
	}
	return candidates, nil
}

func (c *Client) Embedded(ctx context.Context, id int) (domain.Metadata, error) {
	var m domain.Metadata
	err := c.json(ctx, http.MethodGet, bookPath(id)+"/metadata-from-file", nil, &m)
	return m, err
}

type resultStatus struct {
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	FileCounts struct {
		Failed int `json:"failed"`
	} `json:"fileCounts"`
}

func failure(name string, s *resultStatus) error {
	if s != nil && (s.Status == "failed" || s.FileCounts.Failed > 0) {
		return fmt.Errorf("bookorbit: %s failed: %s", name, s.Reason)
	}
	return nil
}

func (c *Client) Apply(ctx context.Context, id int, patch domain.Patch) error {
	var r struct {
		Write *resultStatus `json:"write"`
	}
	if err := c.json(ctx, http.MethodPatch, bookPath(id)+"/metadata?syncFileWrite=true", patch, &r); err != nil {
		return err
	}
	return failure("metadata file write", r.Write)
}

func (c *Client) Cover(ctx context.Context, id int, coverURL string) error {
	return c.json(ctx, http.MethodPost, bookPath(id)+"/cover/from-url", map[string]string{"url": coverURL}, nil)
}

func (c *Client) Write(ctx context.Context, id int) error {
	// The write-and-rename endpoint forces renaming even when disabled in the
	// library. Re-saving the current title triggers the synchronous writer while
	// respecting library settings, keeping all moves under the organizer's journal.
	book, err := c.Book(ctx, id)
	if err != nil {
		return err
	}
	if book.Metadata.Title == "" {
		return errors.New("bookorbit: cannot synchronize a book without a title")
	}
	title, err := json.Marshal(book.Metadata.Title)
	if err != nil {
		return err
	}
	return c.Apply(ctx, id, domain.Patch{"title": title})
}

func (c *Client) Scan(ctx context.Context, id int) error {
	return c.json(ctx, http.MethodPost, "/scanner/libraries/"+strconv.Itoa(id)+"/scan", nil, nil)
}

// Series returns full records for the current series, including other formats
// and books already polished, so covers can be chosen as a coherent set.
func (c *Client) Series(ctx context.Context, book domain.Book) ([]domain.Book, error) {
	if book.Metadata.SeriesName == "" {
		return []domain.Book{book}, nil
	}
	var out []domain.Book
	for page := 0; ; page++ {
		query := struct {
			Filter struct {
				Type  string `json:"type"`
				Join  string `json:"join"`
				Rules []struct {
					Type     string `json:"type"`
					Field    string `json:"field"`
					Operator string `json:"operator"`
					Value    string `json:"value"`
				} `json:"rules"`
			} `json:"filter"`
			Sort       []map[string]string `json:"sort"`
			Pagination map[string]int      `json:"pagination"`
		}{}
		query.Filter.Type = "group"
		query.Filter.Join = "AND"
		query.Filter.Rules = append(query.Filter.Rules, struct {
			Type     string `json:"type"`
			Field    string `json:"field"`
			Operator string `json:"operator"`
			Value    string `json:"value"`
		}{"rule", "series", "eq", book.Metadata.SeriesName})
		query.Sort = []map[string]string{{"field": "seriesIndex", "dir": "asc"}}
		query.Pagination = map[string]int{"page": page, "size": 200}
		var result struct {
			Items []struct{ ID, LibraryID int } `json:"items"`
			Total int                           `json:"total"`
		}
		if err := c.json(ctx, http.MethodPost, "/books/query", query, &result); err != nil {
			return nil, err
		}
		for _, card := range result.Items {
			if card.LibraryID != book.LibraryID {
				continue
			}
			b, err := c.Book(ctx, card.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		}
		if (page+1)*200 >= result.Total {
			break
		}
		if len(result.Items) == 0 {
			return nil, errors.New("bookorbit: incomplete series query")
		}
	}
	for _, b := range out {
		if b.ID == book.ID {
			return out, nil
		}
	}
	return append(out, book), nil
}

func (c *Client) CoverImage(ctx context.Context, id int) ([]byte, error) {
	resp, err := c.request(ctx, http.MethodGet, bookPath(id)+"/cover", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 12<<20))
	return data, err
}
