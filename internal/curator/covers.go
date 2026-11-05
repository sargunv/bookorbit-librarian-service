package curator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/webp"

	"github.com/sargunv/bookorbit-librarian-service/internal/domain"
)

type (
	coverKey  struct{ BookID, Source int }
	coverTile struct {
		key           coverKey
		image         image.Image
		width, height int
	}
)

func (c *Curator) observe(ctx context.Context, evidence []domain.Evidence, currentCover func(context.Context, int) ([]byte, error)) (domain.VisualReport, map[coverKey]bool, error) {
	report := domain.VisualReport{Model: c.visionModel}
	supplied := map[coverKey]bool{}
	var tiles []coverTile
	client := http.Client{Timeout: 30 * time.Second}
	for i := range evidence {
		e := &evidence[i]
		reportProgress(ctx, "covers", i, len(evidence))
		for source := 0; source <= len(e.Candidates); source++ {
			if err := ctx.Err(); err != nil {
				return report, nil, err
			}
			var data []byte
			var err error
			if source == 0 {
				data, err = currentCover(ctx, e.Book.ID)
			} else {
				url := e.Candidates[source-1].CoverURL
				if url == "" {
					continue
				}
				req, x := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				if x != nil {
					continue
				}
				resp, x := client.Do(req)
				if x == nil {
					if resp.StatusCode == 200 {
						data, err = io.ReadAll(io.LimitReader(resp.Body, 12<<20))
					} else {
						err = fmt.Errorf("HTTP %d", resp.StatusCode)
					}
					_ = resp.Body.Close()
				} else {
					err = x
				}
			}
			if err != nil {
				e.Warnings = append(e.Warnings, fmt.Sprintf("cover C%d unavailable", source))
				continue
			}
			config, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil || config.Width*config.Height > 40_000_000 {
				e.Warnings = append(e.Warnings, fmt.Sprintf("cover C%d is not a supported image", source))
				continue
			}
			img, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				continue
			}
			key := coverKey{e.Book.ID, source}
			supplied[key] = true
			scale := min(246.0/float64(config.Width), 364.0/float64(config.Height))
			thumbnail := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(config.Width)*scale)), max(1, int(float64(config.Height)*scale))))
			xdraw.ApproxBiLinear.Scale(thumbnail, thumbnail.Bounds(), img, img.Bounds(), xdraw.Src, nil)
			tiles = append(tiles, coverTile{key, thumbnail, config.Width, config.Height})
		}
	}
	if len(tiles) == 0 {
		report.Summary = "No cover images available; retain current covers."
		return report, supplied, nil
	}
	type caption struct {
		BookID      int      `json:"bookId"`
		Title       string   `json:"title"`
		Authors     []string `json:"authors"`
		SeriesIndex string   `json:"seriesIndex"`
	}
	captions := make([]caption, 0, len(evidence))
	for _, e := range evidence {
		captions = append(captions, caption{e.Book.ID, e.Book.Metadata.Title, e.Book.Metadata.Authors, e.Book.Metadata.SeriesIndex})
	}
	data, err := json.Marshal(captions)
	if err != nil {
		return report, nil, err
	}
	content := []part{{Type: "text", Text: string(data)}}
	for start := 0; start < len(tiles); start += 12 {
		end := min(start+12, len(tiles))
		sheet, err := contactSheet(tiles[start:end])
		if err != nil {
			return report, nil, err
		}
		content = append(content, part{Type: "image_url", ImageURL: &imageURL{URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(sheet)}})
	}
	var observations struct {
		Summary string                    `json:"summary"`
		Covers  []domain.CoverObservation `json:"covers"`
	}
	reportProgress(ctx, "vision", len(tiles), len(tiles))
	u, err := c.call(ctx, c.visionModel, visionInstructions, content, &observations)
	if err != nil {
		return report, nil, err
	}
	seen := map[coverKey]bool{}
	for _, o := range observations.Covers {
		key := coverKey{o.BookID, o.Source}
		if !supplied[key] || seen[key] {
			return report, nil, fmt.Errorf("vision returned unknown or duplicate cover B%d C%d", o.BookID, o.Source)
		}
		seen[key] = true
	}
	if len(seen) != len(supplied) {
		return report, nil, fmt.Errorf("vision omitted covers: got %d of %d", len(seen), len(supplied))
	}
	report.Summary = observations.Summary
	report.Covers = observations.Covers
	report.InputTokens = u.Input
	report.OutputTokens = u.Output
	return report, supplied, nil
}

func contactSheet(tiles []coverTile) ([]byte, error) {
	const tileW, tileH = 256, 390
	cols := min(4, len(tiles))
	rows := (len(tiles) + cols - 1) / cols
	sheet := image.NewRGBA(image.Rect(0, 0, cols*tileW, rows*tileH))
	xdraw.Draw(sheet, sheet.Bounds(), &image.Uniform{C: color.White}, image.Point{}, xdraw.Src)
	for i, t := range tiles {
		x, y := (i%cols)*tileW, (i/cols)*tileH
		d := font.Drawer{Dst: sheet, Src: image.NewUniform(color.Black), Face: basicfont.Face7x13, Dot: fixed.P(x+5, y+14)}
		d.DrawString(fmt.Sprintf("B%d C%d  %dx%d", t.key.BookID, t.key.Source, t.width, t.height))
		w, h := t.width, t.height
		scale := min(float64(tileW-10)/float64(w), float64(tileH-26)/float64(h))
		w, h = max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
		rect := image.Rect(x+(tileW-w)/2, y+22, x+(tileW-w)/2+w, y+22+h)
		xdraw.ApproxBiLinear.Scale(sheet, rect, t.image, t.image.Bounds(), xdraw.Over, nil)
	}
	var out bytes.Buffer
	err := jpeg.Encode(&out, sheet, &jpeg.Options{Quality: 88})
	return out.Bytes(), err
}
