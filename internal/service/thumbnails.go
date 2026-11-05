package service

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sargunv/bookorbit-librarian-service/internal/store"
	xdraw "golang.org/x/image/draw"
)

func (s *Service) coverPath(job int64, version string) string {
	return filepath.Join(filepath.Dir(s.cfg.StatePath), "activity-covers", fmt.Sprintf("%d-%s.jpg", job, version))
}

func (s *Service) hasCover(job int64, version string) bool {
	_, err := os.Stat(s.coverPath(job, version))
	return err == nil
}

// Images are a best-effort audit aid. Thumbnail failures never stop curation.
func (s *Service) captureCover(ctx context.Context, j store.Job, version string) {
	if s.hasCover(j.ID, version) {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _ = s.saveThumbnail(bounded, j, version)
}

func (s *Service) saveThumbnail(ctx context.Context, j store.Job, version string) ([]byte, error) {
	data, err := s.books.CoverImage(ctx, j.BookID)
	if err != nil {
		return nil, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 40_000_000/cfg.Height {
		return nil, fmt.Errorf("cover dimensions exceed limit")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	scale := min(120.0/float64(cfg.Width), 180.0/float64(cfg.Height), 1.0)
	thumbnail := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(cfg.Width)*scale)), max(1, int(float64(cfg.Height)*scale))))
	xdraw.ApproxBiLinear.Scale(thumbnail, thumbnail.Bounds(), img, img.Bounds(), xdraw.Src, nil)
	var out bytes.Buffer
	if err = jpeg.Encode(&out, thumbnail, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	path := s.coverPath(j.ID, version)
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".cover-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	_, err = temp.Write(out.Bytes())
	closeErr := temp.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err = os.Rename(temp.Name(), path); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (s *Service) thumbnail(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeUI(w, r) {
		return
	}
	job, err := strconv.ParseInt(r.PathValue("job"), 10, 64)
	version := r.PathValue("version")
	if err != nil || job <= 0 || (version != "before" && version != "after" && version != "current") {
		http.NotFound(w, r)
		return
	}
	path := s.coverPath(job, version)
	data, err := os.ReadFile(path)
	if version == "current" {
		stat, statErr := os.Stat(path)
		if err != nil || statErr != nil || time.Since(stat.ModTime()) > 5*time.Minute {
			j, getErr := s.db.Get(r.Context(), job)
			if getErr != nil {
				http.NotFound(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			data, err = s.saveThumbnail(ctx, j, version)
		}
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}
