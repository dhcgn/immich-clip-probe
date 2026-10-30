package main

import (
	"bytes"
	"encoding/binary"
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
// handleEncodeClip selects AssetFileType.Preview, whose defaults are JPEG at
// size 1440, quality 80, with EXIF orientation applied first. Every vector in
// smart_search is an embedding of that rendering, so a query image should be
// rendered the same way. See ARCHITECTURE.md section 6.
//
// Immich applies the size to the *short* edge (sharp's `fit: 'outside'`: a
// 4000x3000 original becomes 1920x1440). This deliberately renders the *long*
// edge to 1440 instead: measured on 12 freshly embedded photos it lands closer to
// the stored vectors (mean distance 0.0065 vs 0.0084 at short edge 1440; Immich's
// own preview 0.0055). CatmullRom is not libvips lanczos3, so matching the size
// alone does not buy parity.
const (
	previewLongEdge = 1440
	previewQuality  = 80
)

var errUndecodable = errors.New("not a decodable image")

// normalize renders raw at preview parity. An input that is already an upright
// JPEG within the long-edge limit is returned untouched: re-encoding it would add
// a second generation of JPEG loss for no benefit.
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

	o := 1
	if format == "jpeg" {
		o = jpegOrientation(raw)
	}
	if !enabled || (format == "jpeg" && o == 1 && longEdge(cfg.Width, cfg.Height) <= previewLongEdge) {
		return raw, cfg.Width, cfg.Height, false, nil
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, false, fmt.Errorf("%w: %v", errUndecodable, err)
	}

	// The long edge is the same before and after rotation, so resizing first
	// gives the same result while rotating far fewer pixels.
	img := resize(src, previewLongEdge)
	if o != 1 {
		img = orient(img, o)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: previewQuality}); err != nil {
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

// jpegOrientation reads the EXIF orientation tag (0x0112) from IFD0 of a JPEG's
// APP1 segment. Anything missing, malformed or out of range counts as 1
// (upright): the input is untrusted, and an unrotated query beats a rejected one.
//
// ponytail: JPEG only. PNG/WebP/TIFF can carry EXIF too, but cameras and phones
// write orientation into JPEGs; parse the others if a client ever sends them.
func jpegOrientation(raw []byte) int {
	if len(raw) < 4 || raw[0] != 0xFF || raw[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(raw) && raw[i] == 0xFF; {
		marker := raw[i+1]
		n := int(binary.BigEndian.Uint16(raw[i+2:]))
		if marker == 0xDA || n < 2 || i+2+n > len(raw) {
			return 1 // start of scan, or a broken segment: no EXIF before the pixels
		}
		seg := raw[i+4 : i+2+n]
		if marker == 0xE1 && len(seg) >= 6 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + n
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	ifd := int(bo.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > len(t) {
		return 1
	}
	entries := int(bo.Uint16(t[ifd:]))
	for e := ifd + 2; e+12 <= len(t) && entries > 0; e, entries = e+12, entries-1 {
		if bo.Uint16(t[e:]) == 0x0112 {
			if o := int(bo.Uint16(t[e+8:])); o >= 1 && o <= 8 {
				return o
			}
			return 1
		}
	}
	return 1
}

// orient applies EXIF orientation o (2-8) to the pixels, as Immich's preview
// generator does before embedding: CLIP is not rotation-invariant.
func orient(src image.Image, o int) image.Image {
	b := src.Bounds()
	rgba, ok := src.(*image.RGBA)
	if !ok {
		rgba = image.NewRGBA(b)
		draw.Draw(rgba, b, src, b.Min, draw.Src)
	}
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 { // 5-8 swap width and height
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		for x := range dw {
			sx, sy := x, y
			switch o {
			case 2: // mirror horizontal
				sx = w - 1 - x
			case 3: // rotate 180
				sx, sy = w-1-x, h-1-y
			case 4: // mirror vertical
				sy = h - 1 - y
			case 5: // transpose
				sx, sy = y, x
			case 6: // rotate 90 CW
				sx, sy = y, h-1-x
			case 7: // transverse
				sx, sy = w-1-y, h-1-x
			case 8: // rotate 90 CCW
				sx, sy = w-1-y, x
			}
			copy(dst.Pix[dst.PixOffset(x, y):][:4], rgba.Pix[rgba.PixOffset(b.Min.X+sx, b.Min.Y+sy):][:4])
		}
	}
	return dst
}

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
