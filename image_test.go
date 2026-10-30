package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"slices"
	"testing"
)

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: previewQuality}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A JPEG already within the preview long edge must be passed through untouched.
// Re-encoding it would add a second generation of JPEG loss for no benefit, and
// would shift distances away from what Immich indexed.
func TestNormalizePassesThroughConformingJPEG(t *testing.T) {
	for _, size := range [][2]int{{1200, 900}, {1080, 1440}} {
		in := jpegOf(t, size[0], size[1])

		out, w, h, normalized, err := normalize(in, true)
		if err != nil {
			t.Fatal(err)
		}
		if normalized {
			t.Errorf("%v: normalized = true, want false for a conforming JPEG", size)
		}
		if !bytes.Equal(in, out) {
			t.Errorf("%v: bytes changed: in %d bytes, out %d bytes", size, len(in), len(out))
		}
		if w != size[0] || h != size[1] {
			t.Errorf("dimensions = %dx%d, want %dx%d", w, h, size[0], size[1])
		}
	}
}

// Anything else is rendered to preview parity: JPEG, long edge 1440.
func TestNormalizeRendersToPreviewParity(t *testing.T) {
	for _, tc := range []struct {
		name         string
		in           []byte
		wantW, wantH int
	}{
		{"oversized png", pngOf(t, 4000, 3000), 1440, 1080},
		{"portrait png", pngOf(t, 3000, 4000), 1080, 1440},
		{"oversized jpeg", jpegOf(t, 2880, 2880), 1440, 1440},
		{"small png", pngOf(t, 320, 240), 320, 240}, // re-encoded, never upscaled
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, _, normalized, err := normalize(tc.in, true)
			if err != nil {
				t.Fatal(err)
			}
			if !normalized {
				t.Error("normalized = false, want true")
			}
			cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
			if err != nil {
				t.Fatal(err)
			}
			if format != "jpeg" {
				t.Errorf("format = %q, want jpeg", format)
			}
			if cfg.Width != tc.wantW || cfg.Height != tc.wantH {
				t.Errorf("output = %dx%d, want %dx%d", cfg.Width, cfg.Height, tc.wantW, tc.wantH)
			}
		})
	}
}

// withOrientation inserts an APP1 EXIF segment carrying orientation o right
// after SOI, the way cameras write it.
func withOrientation(jpg []byte, o int, bo binary.ByteOrder) []byte {
	tiff := make([]byte, 26)
	copy(tiff, "MM")
	if bo == binary.LittleEndian {
		copy(tiff, "II")
	}
	bo.PutUint16(tiff[2:], 42)
	bo.PutUint32(tiff[4:], 8)       // IFD0 right after the header
	bo.PutUint16(tiff[8:], 1)       // one entry
	bo.PutUint16(tiff[10:], 0x0112) // Orientation
	bo.PutUint16(tiff[12:], 3)      // SHORT
	bo.PutUint32(tiff[14:], 1)      // count
	bo.PutUint16(tiff[18:], uint16(o))
	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(app1[2:], uint16(2+len(seg)))
	return slices.Concat(jpg[:2], app1, seg, jpg[2:])
}

// markedJPEG is a w×h blue JPEG with a red block in the stored top-left corner
// and a green one in the stored top-right, so where each lands pins the transform.
func markedJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{B: 255, A: 255}
			if y < 12 && x < 12 {
				c = color.RGBA{R: 255, A: 255}
			} else if y < 12 && x >= w-12 {
				c = color.RGBA{G: 255, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Immich applies EXIF orientation before embedding, and CLIP is not
// rotation-invariant: an unrotated query lands far from its own asset. Expected
// corners follow the EXIF spec for where the stored top-left (red) and top-right
// (green) end up when displayed.
func TestNormalizeAppliesOrientation(t *testing.T) {
	const w, h = 64, 40
	type corner int
	const (
		tl corner = iota
		tr
		bl
		br
	)
	for _, tc := range []struct {
		o          int
		red, green corner
	}{
		{1, tl, tr}, {2, tr, tl}, {3, br, bl}, {4, bl, br},
		{5, tl, bl}, {6, tr, br}, {7, br, tr}, {8, bl, tl},
	} {
		for _, bo := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
			in := withOrientation(markedJPEG(t, w, h), tc.o, bo)
			if got := jpegOrientation(in); got != tc.o {
				t.Fatalf("orientation %d (%v): read %d", tc.o, bo, got)
			}
			out, sw, sh, normalized, err := normalize(in, true)
			if err != nil {
				t.Fatal(err)
			}
			if sw != w || sh != h {
				t.Errorf("orientation %d: reported %dx%d, want the submitted %dx%d", tc.o, sw, sh, w, h)
			}
			if normalized != (tc.o != 1) {
				t.Errorf("orientation %d: normalized = %v", tc.o, normalized)
			}
			img, err := jpeg.Decode(bytes.NewReader(out))
			if err != nil {
				t.Fatal(err)
			}
			b := img.Bounds()
			wantW := w
			if tc.o >= 5 {
				wantW = h
			}
			if b.Dx() != wantW {
				t.Errorf("orientation %d: output %dx%d, want width %d", tc.o, b.Dx(), b.Dy(), wantW)
			}
			at := func(c corner) color.Color {
				x, y := 4, 4
				if c == tr || c == br {
					x = b.Dx() - 5
				}
				if c == bl || c == br {
					y = b.Dy() - 5
				}
				return img.At(x, y)
			}
			if r, g, bb, _ := at(tc.red).RGBA(); r>>8 < 200 || g>>8 > 80 || bb>>8 > 80 {
				t.Errorf("orientation %d (%v): red corner is %v", tc.o, bo, at(tc.red))
			}
			if r, g, bb, _ := at(tc.green).RGBA(); r>>8 > 80 || g>>8 < 200 || bb>>8 > 80 {
				t.Errorf("orientation %d (%v): green corner is %v", tc.o, bo, at(tc.green))
			}
		}
	}
}

// The EXIF parser reads untrusted bytes: every truncation of a valid file must
// come back as "upright" or the real value, never a panic.
func TestJPEGOrientationSurvivesTruncation(t *testing.T) {
	in := withOrientation(jpegOf(t, 8, 8), 6, binary.LittleEndian)
	for n := range len(in) {
		if o := jpegOrientation(in[:n]); o != 1 && o != 6 {
			t.Fatalf("truncated to %d bytes: orientation %d", n, o)
		}
	}
	if o := jpegOrientation(withOrientation(jpegOf(t, 8, 8), 42, binary.BigEndian)); o != 1 {
		t.Errorf("out-of-range orientation read as %d, want 1", o)
	}
}

// PREVIEW_PARITY=false hands the ML container exactly what was submitted.
func TestNormalizeDisabled(t *testing.T) {
	in := pngOf(t, 4000, 3000)
	out, _, _, normalized, err := normalize(in, false)
	if err != nil {
		t.Fatal(err)
	}
	if normalized || !bytes.Equal(in, out) {
		t.Error("parity disabled must pass the input through untouched")
	}
}

// HEIC, RAW and garbage must produce a clean 415 rather than reaching the ML
// container.
func TestNormalizeRejectsUndecodable(t *testing.T) {
	_, _, _, _, err := normalize([]byte("ftypheic not really an image"), true)
	if !errors.Is(err, errUndecodable) {
		t.Errorf("err = %v, want errUndecodable", err)
	}
}

// The health probe must itself be a decodable image, or /healthz reports a
// mismatch that does not exist.
func TestProbeJPEGIsValid(t *testing.T) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(probeJPEG))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" || cfg.Width == 0 || cfg.Height == 0 {
		t.Errorf("probe = %s %dx%d", format, cfg.Width, cfg.Height)
	}
}
