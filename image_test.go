package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
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
	in := jpegOf(t, 1200, 900)

	out, w, h, normalized, err := normalize(in, true)
	if err != nil {
		t.Fatal(err)
	}
	if normalized {
		t.Error("normalized = true, want false for a conforming JPEG")
	}
	if !bytes.Equal(in, out) {
		t.Errorf("bytes changed: in %d bytes, out %d bytes", len(in), len(out))
	}
	if w != 1200 || h != 900 {
		t.Errorf("dimensions = %dx%d, want 1200x900", w, h)
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
