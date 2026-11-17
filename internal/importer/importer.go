// Package importer provides private, durable Adobe import jobs.
package importer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const MaxBytes int64 = 250 << 20

type Job struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Extension string    `json:"extension"`
	Hash      string    `json:"hash"`
	Status    string    `json:"status"`
	Message   string    `json:"message"`
	Session   string    `json:"-"`
	BookID    int       `json:"bookId,omitempty"`
	Created   time.Time `json:"created"`
}

// diskJob includes private recovery state omitted from HTTP responses.
type diskJob struct {
	Job
	SessionID string `json:"sessionId,omitempty"`
}
type (
	Upload  func(context.Context, string, string, string, string, func(string) error) (int, error)
	Manager struct {
		root, command, secrets, account string
		mu                              sync.Mutex
		upload                          Upload
		enqueue                         func(context.Context, int) error
	}
)

func New(root, command, secrets, account string, upload Upload, enqueue func(context.Context, int) error) (*Manager, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Manager{root: root, command: command, secrets: secrets, account: account, upload: upload, enqueue: enqueue}, nil
}
func (m *Manager) path(id string) string { return filepath.Join(m.root, id) }
func (m *Manager) save(j Job) error {
	data, e := json.Marshal(diskJob{Job: j, SessionID: j.Session})
	if e != nil {
		return e
	}
	path := filepath.Join(m.path(j.ID), "job.json")
	f, e := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if e != nil {
		return e
	}
	_, e = f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(path+".tmp", path); e != nil {
		return e
	}
	dir, e := os.Open(m.path(j.ID))
	if e != nil {
		return e
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (m *Manager) jobs() ([]Job, error) {
	entries, e := os.ReadDir(m.root)
	if e != nil {
		return nil, e
	}
	jobs := []Job{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, e := os.ReadFile(filepath.Join(m.root, entry.Name(), "job.json"))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		var d diskJob
		if e = json.Unmarshal(data, &d); e != nil {
			return nil, e
		}
		if d.ID != entry.Name() {
			return nil, errors.New("invalid import record")
		}
		d.Session = d.SessionID
		jobs = append(jobs, d.Job)
	}
	slices.SortFunc(jobs, func(a, b Job) int { return b.Created.Compare(a.Created) })
	return jobs, nil
}
func validID(id string) bool { b, e := hex.DecodeString(id); return e == nil && len(b) == 16 }
func (m *Manager) Create(name string, body io.Reader) (Job, error) {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if len(name) > 240 {
		name = "Book" + filepath.Ext(name)
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".acsm" && ext != ".epub" && ext != ".pdf" {
		return Job{}, errors.New("choose an ACSM, EPUB or PDF file")
	}
	id := make([]byte, 16)
	if _, e := rand.Read(id); e != nil {
		return Job{}, e
	}
	j := Job{ID: hex.EncodeToString(id), Name: name, Extension: ext, Status: "queued", Created: time.Now().UTC()}
	if e := os.Mkdir(m.path(j.ID), 0o700); e != nil {
		return Job{}, e
	}
	f, e := os.OpenFile(filepath.Join(m.path(j.ID), "source"+ext), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if e != nil {
		return Job{}, e
	}
	hash := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, hash), io.LimitReader(body, MaxBytes+1))
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return Job{}, e
	}
	if closeErr != nil {
		return Job{}, closeErr
	}
	if n == 0 || n > MaxBytes {
		return Job{}, errors.New("file must be between 1 byte and 250 MiB")
	}
	j.Hash = hex.EncodeToString(hash.Sum(nil))
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, e := m.jobs()
	if e != nil {
		return Job{}, e
	}
	for _, old := range existing {
		if old.Hash == j.Hash {
			return old, nil
		}
	}
	return j, m.save(j)
}

func (m *Manager) Retry(id string) error {
	if !validID(id) {
		return errors.New("invalid import ID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	jobs, e := m.jobs()
	if e != nil {
		return e
	}
	for _, j := range jobs {
		if j.ID == id && j.Status == "failed" {
			j.Status = "queued"
			j.Message = ""
			return m.save(j)
		}
	}
	return errors.New("failed import not found")
}

func (m *Manager) Run(ctx context.Context) {
	// A single worker owns recovery state and the mutable Adobe activation copy.
	for {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		jobs, e := m.jobs()
		m.mu.Unlock()
		if e == nil {
			for i := len(jobs) - 1; i >= 0; i-- {
				j := jobs[i]
				if j.Status == "queued" || j.Status == "processing" {
					m.process(ctx, j)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (m *Manager) process(ctx context.Context, j Job) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	update := func() error { m.mu.Lock(); defer m.mu.Unlock(); return m.save(j) }
	j.Status = "processing"
	j.Message = "Preparing book"
	if update() != nil {
		return
	}
	fail := func(message string) { j.Status = "failed"; j.Message = message; _ = update() }
	work := m.path(j.ID)
	if j.BookID == 0 {
		cmd := exec.CommandContext(ctx, "python3", m.command, filepath.Join(work, "source"+j.Extension), work, m.secrets, m.account)
		// Capture no stderr: upstream exceptions may contain private source data.
		result, e := cmd.Output()
		if e != nil {
			if ctx.Err() != nil {
				fail("Processing interrupted or timed out. Retry resumes saved progress.")
			} else {
				fail("Adobe processing failed. Check file format, ADE keys, activation, or distributor availability.")
			}
			return
		}
		var output struct {
			Filename string `json:"filename"`
		}
		if json.Unmarshal(result, &output) != nil || (output.Filename != "book.epub" && output.Filename != "book.pdf") {
			fail("Invalid processor output")
			return
		}
		j.Message = "Adding to BookOrbit"
		if update() != nil {
			return
		}
		filename := strings.TrimSuffix(j.Name, filepath.Ext(j.Name)) + filepath.Ext(output.Filename)
		book, e := m.upload(ctx, j.ID, filepath.Join(work, output.Filename), filename, j.Session, func(id string) error { j.Session = id; return update() })
		if e != nil {
			fail("BookOrbit upload did not finish. Retry resumes the saved upload session.")
			return
		}
		j.BookID = book
		if update() != nil {
			return
		}
	}
	if e := m.enqueue(ctx, j.BookID); e != nil {
		fail("Book imported; curation could not be queued. Retry to queue curation.")
		return
	}
	j.Status = "completed"
	j.Message = "Imported; metadata curation queued"
	_ = update()
}

// Mount grants a narrow import capability behind the existing authenticated UI.
// The custom header and Origin check prevent cross-site form submissions.
func (m *Manager) Mount(mux *http.ServeMux, authorize func(http.ResponseWriter, *http.Request) bool) {
	mux.HandleFunc("GET /imports/jobs", func(w http.ResponseWriter, r *http.Request) {
		if !authorize(w, r) {
			return
		}
		m.mu.Lock()
		jobs, e := m.jobs()
		m.mu.Unlock()
		if e != nil {
			http.Error(w, "imports unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(jobs)
	})
	mutation := func(w http.ResponseWriter, r *http.Request) bool {
		if !authorize(w, r) {
			return false
		}
		if r.Header.Get("X-Librarian-Import") != "1" {
			http.Error(w, "missing import header", http.StatusForbidden)
			return false
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, e := url.Parse(origin)
			if e != nil || u.Host != r.Host {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return false
			}
		}
		return true
	}
	mux.HandleFunc("POST /imports/upload", func(w http.ResponseWriter, r *http.Request) {
		if !mutation(w, r) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBytes+(1<<20))
		reader, e := r.MultipartReader()
		if e != nil {
			http.Error(w, "expected file upload", 400)
			return
		}
		part, e := reader.NextPart()
		if e != nil || part.FileName() == "" {
			http.Error(w, "expected file", 400)
			return
		}
		j, e := m.Create(part.FileName(), part)
		if e != nil {
			http.Error(w, "Import rejected. Choose an ACSM, EPUB or PDF file up to 250 MiB.", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(j)
	})
	mux.HandleFunc("POST /imports/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		if !mutation(w, r) {
			return
		}
		if e := m.Retry(r.PathValue("id")); e != nil {
			http.Error(w, "import cannot be retried", http.StatusConflict)
			return
		}
		w.WriteHeader(202)
	})
}
