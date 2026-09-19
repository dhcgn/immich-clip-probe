package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"

	_ "image/gif"
	_ "image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// Immich embeds the *preview* derivative of an asset, not the original:
// handleEncodeClip selects AssetFileType.Preview, whose defaults are JPEG with a
// long edge of 1440 at quality 80. Every vector in smart_search is therefore an
// embedding of that rendering, so a query image should be rendered the same way.
// See ARCHITECTURE.md section 6.
const (
	previewLongEdge = 1440
	previewQuality  = 80
)

var errUndecodable = errors.New("not a decodable image")

// normalize renders raw at preview parity. An input that is already JPEG within
// the long-edge limit is returned untouched: re-encoding it would add a second
// generation of JPEG loss for no benefit.
//
// Returns the bytes to embed, the dimensions of the image as submitted, and
// whether anything was re-encoded.
func normalize(raw []byte, enabled bool) (out []byte, width, height int, normalized bool, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("%w: %v", errUndecodable, err)
	}
	if cfg.Width == 0 || cfg.Height == 0 {
		return nil, 0, 0, false, errors.New("image has zero width or height")
	}

	if !enabled || (format == "jpeg" && longEdge(cfg.Width, cfg.Height) <= previewLongEdge) {
		return raw, cfg.Width, cfg.Height, false, nil
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("%w: %v", errUndecodable, err)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, resize(src, previewLongEdge), &jpeg.Options{Quality: previewQuality}); err != nil {
		return nil, 0, 0, false, err
	}
	return buf.Bytes(), cfg.Width, cfg.Height, true, nil
}

// resize scales src down so its long edge is at most longEdgePx. Images already
// within the limit are returned as-is; nothing is ever upscaled.
func resize(src image.Image, longEdgePx int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if longEdge(w, h) <= longEdgePx {
		return src
	}

	var nw, nh int
	if w >= h {
		nw, nh = longEdgePx, h*longEdgePx/w
	} else {
		nw, nh = w*longEdgePx/h, longEdgePx
	}
	nw, nh = max(nw, 1), max(nh, 1)

	// ponytail: CatmullRom, not Immich's libvips lanczos3 + P3 colorspace. The
	// residual bias sits well under the duplicate threshold; if calibration ever
	// shows drift, shell out to vips or embed both renderings and take the min.
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

func longEdge(w, h int) int { return max(w, h) }

// probeJPEG is a trivial valid image used only to ask the ML container for an
// embedding whose width can be compared against smart_search. Generated rather
// than embedded as a byte blob so it stays obviously harmless.
var probeJPEG = func() []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 16, 16)), nil); err != nil {
		panic(err) // a 16x16 gray JPEG cannot fail to encode
	}
	return buf.Bytes()
}()
