package bookorbit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"time"
)

type UploadSession struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Received  int64  `json:"receivedBytes"`
	ChunkSize int64  `json:"chunkSizeBytes"`
	BookID    int    `json:"bookId"`
}

// UploadFile resumes a persisted upstream session. save must durably record its
// ID before any bytes are sent; completed sessions are never uploaded again.
func (c *Client) UploadFile(ctx context.Context, key string, library int, path, filename, sessionID string, save func(string) error) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, errors.New("cannot open import output")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	var session UploadSession
	if sessionID == "" {
		hash := sha256.New()
		if _, err = io.Copy(hash, f); err != nil {
			return 0, err
		}
		payload := map[string]any{"filename": filename, "sizeBytes": info.Size(), "sha256": hex.EncodeToString(hash.Sum(nil)), "idempotencyKey": "librarian-" + key, "target": map[string]any{"kind": "library", "libraryId": library}}
		if err = c.json(ctx, "POST", "/uploads", payload, &session); err != nil {
			return 0, err
		}
		if session.ID == "" {
			return 0, errors.New("upload session missing ID")
		}
		if err = save(session.ID); err != nil {
			return 0, err
		}
	} else if err = c.json(ctx, "GET", "/uploads/"+sessionID, nil, &session); err != nil {
		return 0, err
	}
	for {
		switch session.Status {
		case "completed":
			if session.BookID < 1 {
				return 0, errors.New("upload completed without book ID")
			}
			return session.BookID, nil
		case "processing":
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(time.Second):
			}
			if err = c.json(ctx, "GET", "/uploads/"+session.ID, nil, &session); err != nil {
				return 0, err
			}
		case "receiving":
			if session.Received < 0 || session.Received > info.Size() || session.ChunkSize < 1 || session.ChunkSize > 16<<20 {
				return 0, errors.New("invalid upload offset or chunk size")
			}
			if session.Received == info.Size() {
				if err = c.json(ctx, "POST", "/uploads/"+session.ID+"/complete", nil, &session); err != nil {
					return 0, err
				}
				continue
			}
			if _, err = f.Seek(session.Received, io.SeekStart); err != nil {
				return 0, err
			}
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, e := writer.CreateFormFile("file", filename)
			if e != nil {
				return 0, e
			}
			if _, err = io.CopyN(part, f, min(session.ChunkSize, info.Size()-session.Received)); err != nil {
				return 0, err
			}
			if err = writer.Close(); err != nil {
				return 0, err
			}
			if err = c.uploadChunk(ctx, session.ID, session.Received, writer.FormDataContentType(), body.Bytes(), &session); err != nil {
				return 0, err
			}
		default:
			return 0, errors.New("upstream upload failed or expired; inspect before creating another import")
		}
	}
}

func (c *Client) uploadChunk(ctx context.Context, id string, offset int64, contentType string, body []byte, out any) error {
	token, err := c.authenticate(ctx, "")
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, e := http.NewRequestWithContext(ctx, "POST", c.base+"/uploads/"+id+"/chunks", bytes.NewReader(body))
		if e != nil {
			return e
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
		resp, e := c.http.Do(req)
		if e != nil {
			return errors.New("upload chunk transport failed; retry resumes from server offset")
		}
		if resp.StatusCode == 401 && attempt == 0 {
			_ = resp.Body.Close()
			token, err = c.authenticate(ctx, token)
			if err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			return fmt.Errorf("upload chunk HTTP %d", resp.StatusCode)
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out)
		_ = resp.Body.Close()
		return err
	}
	return errors.New("upload chunk unauthorized")
}
