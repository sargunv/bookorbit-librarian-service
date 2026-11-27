package bookorbit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"strconv"
)

// UploadPending marks a multipart upload whose outcome is not yet known.
const UploadPending = "multipart-pending"

// ErrUploadUnknown means an earlier attempt may have created a book. Repeating
// it automatically could add a duplicate.
var ErrUploadUnknown = errors.New("bookorbit: earlier upload outcome unknown")

// UploadFile adds a file through the library multipart upload used by
// BookOrbit's web app. Resumable /uploads sessions are reserved for attested
// official apps since BookOrbit 3.7.
//
// The route has no idempotency key, so mark must durably record UploadPending
// before any bytes are sent. A definite rejection clears the mark. A lost
// response keeps it, and any recorded state makes UploadFile refuse to send
// again: legacy session IDs and pending attempts both need inspection.
func (c *Client) UploadFile(ctx context.Context, library int, path, filename, state string, mark func(string) error) (int, error) {
	if state != "" {
		return 0, ErrUploadUnknown
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, errors.New("cannot open import output")
	}
	defer func() { _ = f.Close() }()
	token, err := c.authenticate(ctx, "")
	if err != nil {
		return 0, err
	}
	if err = mark(UploadPending); err != nil {
		return 0, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		body, writer := io.Pipe()
		form := multipart.NewWriter(writer)
		done := make(chan struct{})
		go func() {
			defer close(done)
			part, e := form.CreateFormFile("file", filename)
			if e == nil {
				_, e = io.Copy(part, f)
			}
			if e == nil {
				e = form.Close()
			}
			_ = writer.CloseWithError(e)
		}()
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/libraries/"+strconv.Itoa(library)+"/upload", body)
		if e != nil {
			_ = body.Close()
			<-done
			return 0, errors.New("bookorbit: invalid upload URL")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", form.FormDataContentType())
		resp, e := c.http.Do(req)
		// The writer must stop reading f before a retry rewinds it.
		_ = body.Close()
		<-done
		if e != nil {
			if notSent(e) {
				return 0, errors.Join(mark(""), fmt.Errorf("bookorbit: upload not sent: %w", safeTransport(e)))
			}
			return 0, fmt.Errorf("bookorbit: upload response lost: %w", safeTransport(e))
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_ = resp.Body.Close()
			if token, err = c.authenticate(ctx, token); err != nil {
				return 0, errors.Join(mark(""), err)
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			// BookOrbit removes stored bytes before it answers with an error.
			return 0, errors.Join(mark(""), fmt.Errorf("bookorbit: upload HTTP %d", resp.StatusCode))
		}
		var result struct {
			BookID int `json:"bookId"`
		}
		e = json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&result)
		_ = resp.Body.Close()
		if e != nil || result.BookID < 1 {
			return 0, errors.New("bookorbit: upload accepted without a book ID")
		}
		return result.BookID, nil
	}
	return 0, errors.Join(mark(""), errors.New("bookorbit: upload unauthorized"))
}

// notSent recognizes failures before a connection exists, when BookOrbit
// cannot have received the file.
func notSent(err error) bool {
	var op *net.OpError
	var dns *net.DNSError
	return errors.As(err, &dns) || (errors.As(err, &op) && op.Op == "dial")
}
