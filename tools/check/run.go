package check

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Status string

const (
	StatusMatch   Status = "match"    // nearest distance ≤ max_distance
	StatusNoMatch Status = "no_match" // nearest is further away, or nothing is indexed
	StatusSkipped Status = "skipped"
	StatusError   Status = "error"
)

// Result is the outcome for one file. Distance is the nearest match's, and is
// set whenever the server answered with any match at all.
type Result struct {
	Path       string   `json:"path"`
	Status     Status   `json:"status"`
	Distance   *float64 `json:"distance,omitempty"`
	Matches    []Match  `json:"matches,omitempty"`
	RenderedBy string   `json:"renderedBy,omitempty"` // "imagemagick", or "server" when sent as-is
	Error      string   `json:"error,omitempty"`
}

// Immich's image extensions, RAW included (server/src/utils/mime-types.ts).
var imageExts = exts(".3fr .ari .arw .cap .cin .cr2 .cr3 .crw .dcr .dng .erf .fff .iiq .k25 .kdc .mrw .nef .nrw .orf .ori .pef .psd .raf .raw .rw2 .rwl .sr2 .srf .srw .x3f " +
	".avif .bmp .gif .heic .heif .hif .insp .jfif .jp2 .jpe .jpeg .jpg .jxl .mpo .png .svg .tif .tiff .webp")

// What clip-probe decodes itself (Go's image packages), so it can be sent
// without ImageMagick.
var serverExts = exts(".jpg .jpeg .jpe .jfif .png .gif .webp .tif .tiff")

func exts(list string) map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(list) {
		m[e] = true
	}
	return m
}

func ext(path string) string { return strings.ToLower(filepath.Ext(path)) }

// Collect expands paths into the files to check. Folders are walked
// recursively for Immich image types, skipping dotfiles and dot-folders
// (sidecars, thumbnail caches); a file named explicitly is always kept.
func Collect(paths []string) ([]string, error) {
	var out []string
	for _, root := range paths {
		fi, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			out = append(out, root)
			continue
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p != root && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.IsDir() && imageExts[ext(p)] {
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Run checks files with cfg.Workers in parallel and calls onResult once per
// file, never concurrently, in completion order. Once ctx is cancelled no more
// files start and in-flight ones are dropped rather than reported as errors.
// The error is for configuration only; per-file failures are Results.
func Run(ctx context.Context, cfg Config, files []string, onResult func(Result)) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	c := NewClient(cfg.Server, cfg.Token)
	magick := FindMagick(cfg.Magick)

	jobs := make(chan string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range max(cfg.Workers, 1) {
		wg.Go(func() {
			for path := range jobs {
				r := checkOne(ctx, c, magick, cfg, path)
				if ctx.Err() != nil {
					continue
				}
				mu.Lock()
				onResult(r)
				mu.Unlock()
			}
		})
	}
feed:
	for _, f := range files {
		select {
		case jobs <- f:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return nil
}

func checkOne(ctx context.Context, c *Client, magick string, cfg Config, path string) Result {
	r := Result{Path: path}
	var img []byte
	var err error
	switch {
	case magick != "":
		r.RenderedBy = "imagemagick"
		img, err = Render(ctx, magick, path)
	case serverExts[ext(path)]:
		r.RenderedBy = "server"
		img, err = os.ReadFile(path)
	default:
		r.Status, r.Error = StatusSkipped, "needs ImageMagick to decode "+ext(path)
		return r
	}
	if err != nil {
		r.Status, r.Error = StatusError, err.Error()
		return r
	}

	r.Matches, err = c.Similar(ctx, filepath.Base(path), img, cfg.Limit)
	if err != nil {
		r.Status, r.Error = StatusError, err.Error()
		return r
	}
	r.Status = StatusNoMatch
	if len(r.Matches) > 0 {
		d := r.Matches[0].Distance
		r.Distance = &d
		if d <= cfg.MaxDistance {
			r.Status = StatusMatch
		}
	}
	return r
}
