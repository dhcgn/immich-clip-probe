package check

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func writeJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeProbe answers like clip-probe: a file whose name starts with "dup" is
// a near-duplicate, anything else is far away. It records every upload.
type fakeProbe struct {
	mu      sync.Mutex
	queries []string // url query of each request
	names   []string
	bodies  [][]byte
	keys    []string
}

func (f *fakeProbe) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, hdr, err := r.FormFile("file")
		if err != nil {
			t.Errorf("no file part: %v", err)
			return
		}
		body, _ := io.ReadAll(file)
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.RawQuery)
		f.names = append(f.names, hdr.Filename)
		f.bodies = append(f.bodies, body)
		f.keys = append(f.keys, r.Header.Get("x-api-key"))
		f.mu.Unlock()
		d := "0.19"
		if strings.HasPrefix(hdr.Filename, "dup") {
			d = "0.004"
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"matches":[{"assetId":"a1","originalFileName":"IMG_1.HEIC","distance":`+d+`,"links":{"web":"https://immich/photos/a1"}},{"assetId":"a2","distance":0.3}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The client asks for the nearest matches without a threshold, so a distance
// beyond max_distance still comes back, and authenticates with x-api-key.
func TestSimilarAsksForNearestWithoutThreshold(t *testing.T) {
	var f fakeProbe
	srv := f.serve(t)
	m, err := NewClient(srv.URL+"/", "tok").Similar(context.Background(), "x.jpg", []byte("img"), 3)
	if err != nil {
		t.Fatal(err)
	}
	q := f.queries[0]
	for _, want := range []string{"all=true", "limit=3", "type=IMAGE"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %s", q, want)
		}
	}
	if strings.Contains(q, "max_distance") {
		t.Errorf("query %q sends a threshold; distances beyond it would be lost", q)
	}
	if f.keys[0] != "tok" || f.names[0] != "x.jpg" || string(f.bodies[0]) != "img" {
		t.Errorf("upload = key %q name %q body %q", f.keys[0], f.names[0], f.bodies[0])
	}
	if len(m) != 2 || m[0].Distance != 0.19 || m[0].Links.Web != "https://immich/photos/a1" {
		t.Errorf("matches = %+v", m)
	}
}

func TestErrorsCarryTheServerCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		io.WriteString(w, `{"error":"unsupported_media","message":"not a decodable image"}`)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "tok").Similar(context.Background(), "x", nil, 1)
	if e, ok := errors.AsType[*APIError](err); !ok || e.Code != "unsupported_media" || e.Status != 415 {
		t.Errorf("err = %#v, want APIError unsupported_media/415", err)
	}
}

func TestCollect(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"a.jpg", "b.HEIC", "c.mov", "notes.txt", ".hidden.jpg", "sub/d.png", ".thumbs/e.jpg"} {
		writeJPEG(t, filepath.Join(dir, p), 1, 1)
	}
	explicit := filepath.Join(dir, "notes.txt")
	got, err := Collect([]string{dir, explicit})
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		got[i], _ = filepath.Rel(dir, got[i])
		got[i] = filepath.ToSlash(got[i])
	}
	want := []string{"a.jpg", "b.HEIC", "sub/d.png", "notes.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("Collect = %v, want %v: folders keep Immich image types minus dotfiles, explicit files stay", got, want)
	}
	if _, err := Collect([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("a missing path must be an error, not an empty result")
	}
}

func runAll(t *testing.T, cfg Config, files []string) map[string]Result {
	t.Helper()
	out := map[string]Result{}
	if err := Run(context.Background(), cfg, files, func(r Result) { out[filepath.Base(r.Path)] = r }); err != nil {
		t.Fatal(err)
	}
	return out
}

// Without ImageMagick, files clip-probe decodes go up as they are and the
// match is decided here against max_distance; a HEIC is skipped, not failed.
func TestRunWithoutMagick(t *testing.T) {
	var f fakeProbe
	srv := f.serve(t)
	dir := t.TempDir()
	for _, n := range []string{"dup.jpg", "other.jpg", "phone.heic"} {
		writeJPEG(t, filepath.Join(dir, n), 8, 8)
	}
	files, _ := Collect([]string{dir})
	cfg := Config{Server: srv.URL, Token: "tok", MaxDistance: 0.01, Limit: 2, Workers: 2, Magick: "off"}
	got := runAll(t, cfg, files)

	if r := got["dup.jpg"]; r.Status != StatusMatch || *r.Distance != 0.004 || r.RenderedBy != "server" || len(r.Matches) != 2 {
		t.Errorf("dup.jpg = %+v, want a match at 0.004 sent as-is", r)
	}
	if r := got["other.jpg"]; r.Status != StatusNoMatch || *r.Distance != 0.19 {
		t.Errorf("other.jpg = %+v, want no_match with the distance kept", r)
	}
	if r := got["phone.heic"]; r.Status != StatusSkipped || !strings.Contains(r.Error, "ImageMagick") {
		t.Errorf("phone.heic = %+v, want skipped for want of ImageMagick", r)
	}
	if len(f.names) != 2 {
		t.Errorf("%d uploads, want 2: the HEIC is never sent", len(f.names))
	}
	cfg.MaxDistance = 0.2
	if r := runAll(t, cfg, files)["other.jpg"]; r.Status != StatusMatch {
		t.Errorf("at 0.2 other.jpg = %s, want match", r.Status)
	}
}

func TestRunNeedsServerAndToken(t *testing.T) {
	if err := Run(context.Background(), Config{Token: "t"}, nil, nil); err == nil {
		t.Error("no server must be refused before any work")
	}
}

// withOrientation inserts an EXIF APP1 segment carrying orientation o right
// after the JPEG's SOI marker. ImageMagick's -orient writes none into a JPEG
// that has no EXIF profile yet, so tests build it by hand.
func withOrientation(jpg []byte, o byte) []byte {
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, // big-endian header, IFD0 at 8
		0, 1, // one entry
		0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, o, 0, 0, // Orientation, SHORT, count 1, value o
		0, 0, 0, 0} // no next IFD
	seg := append([]byte{0xFF, 0xE1, 0, byte(2 + 6 + len(tiff))}, "Exif\x00\x00"...)
	seg = append(seg, tiff...)
	return append(append(jpg[:2:2], seg...), jpg[2:]...)
}

func needMagick(t *testing.T) string {
	t.Helper()
	m := FindMagick("")
	if m == "" {
		t.Skip("ImageMagick not installed")
	}
	return m
}

// A rotated camera original comes out upright, long edge 1440, as a JPEG
// clip-probe passes through untouched (orientation reads as top-left).
func TestRenderOrientsAndSizes(t *testing.T) {
	magick := needMagick(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jpg")
	// 4000x3000, red top-left corner, stored with EXIF orientation 6 (rotate 90° cw).
	pix := image.NewRGBA(image.Rect(0, 0, 4000, 3000))
	draw.Draw(pix, pix.Bounds(), &image.Uniform{color.RGBA{B: 255, A: 255}}, image.Point{}, draw.Src)
	draw.Draw(pix, image.Rect(0, 0, 400, 300), &image.Uniform{color.RGBA{R: 255, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, pix, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, withOrientation(buf.Bytes(), 6), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := Render(context.Background(), magick, src)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 1080 || b.Dy() != 1440 {
		t.Fatalf("rendered %v, want 1080x1440: upright portrait, long edge 1440", b)
	}
	// Rotating 90° cw moves the stored top-left corner to the top-right.
	if r, g, b, _ := img.At(1070, 10).RGBA(); r>>8 < 200 || g>>8 > 60 || b>>8 > 60 {
		t.Errorf("top-right pixel = %d,%d,%d, want red: orientation not applied", r>>8, g>>8, b>>8)
	}
	rendered := filepath.Join(dir, "out.jpg")
	if err := os.WriteFile(rendered, out, 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := exec.Command(magick, "identify", "-format", "%[orientation]", rendered).Output()
	if err != nil {
		t.Fatal(err)
	}
	if s := string(o); s != "TopLeft" && s != "Undefined" {
		t.Errorf("rendered orientation %q; clip-probe would rotate it a second time", s)
	}

	if _, err := Render(context.Background(), magick, filepath.Join(dir, "missing.jpg")); err == nil || !strings.HasPrefix(err.Error(), "imagemagick: ") {
		t.Errorf("missing file: err = %v, want an imagemagick error", err)
	}
}

// With ImageMagick, every file is rendered locally, HEIC and a small upright
// JPEG alike: Immich re-encodes every original into its preview too.
func TestRunWithMagickRendersEverything(t *testing.T) {
	magick := needMagick(t)
	var f fakeProbe
	srv := f.serve(t)
	dir := t.TempDir()
	writeJPEG(t, filepath.Join(dir, "dup.jpg"), 3000, 2000)
	files := []string{filepath.Join(dir, "dup.jpg")}
	if exec.Command(magick, "-size", "64x48", "xc:green", filepath.Join(dir, "phone.heic")).Run() == nil {
		files = append(files, filepath.Join(dir, "phone.heic"))
	}
	got := runAll(t, Config{Server: srv.URL, Token: "tok", MaxDistance: 0.01, Limit: 1, Workers: 1, Magick: magick}, files)
	if r := got["dup.jpg"]; r.Status != StatusMatch || r.RenderedBy != "imagemagick" {
		t.Errorf("dup.jpg = %+v, want a match rendered by imagemagick", r)
	}
	if len(files) == 2 {
		if r := got["phone.heic"]; r.Status != StatusNoMatch {
			t.Errorf("phone.heic = %+v, want it decoded and checked", r)
		}
	}
	for i, b := range f.bodies {
		cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
		if err != nil || format != "jpeg" || max(cfg.Width, cfg.Height) > 1440 {
			t.Errorf("upload %s: %s %dx%d (%v), want a JPEG within 1440", f.names[i], format, cfg.Width, cfg.Height, err)
		}
	}
}

func TestConfigPrecedence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte("server: https://probe\nlimit: 5\nworkers: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIP_CHECK_LIMIT", "7")
	t.Setenv("CLIP_CHECK_TOKEN", "from-env")
	v, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	v.Set("workers", 9) // what a flag does
	c, err := Decode(v)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Server: "https://probe", Token: "from-env", MaxDistance: 0.01, Limit: 7, Workers: 9}
	if c != want {
		t.Errorf("config = %+v, want %+v (default < file < env < flag)", c, want)
	}

	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err != nil {
		t.Errorf("a missing file must not be an error: %v", err)
	}

	saved := filepath.Join(t.TempDir(), "new", "config.yaml")
	if err := Save(saved, Config{Server: "s", Token: "t", MaxDistance: 0.02, Limit: 1, Workers: 2, Magick: "off"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIP_CHECK_LIMIT", "")
	t.Setenv("CLIP_CHECK_TOKEN", "")
	v, _ = Load(saved)
	if c, _ := Decode(v); c.MaxDistance != 0.02 || c.Magick != "off" || c.Token != "t" {
		t.Errorf("round trip = %+v", c)
	}
}
