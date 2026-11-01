package check

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// magickArgs render a file the way Immich renders the preview it embeds
// (server/src/repositories/media.repository.ts, Immich 3.3.0 and later):
// EXIF orientation applied, resampled in linear light with lanczos3, JPEG
// quality 80 with 4:4:4 chroma, colour profile left alone (sRGB stays sRGB,
// Display P3 stays P3). Immich always re-encodes the original into that
// preview, so re-encoding here is parity, not a second generation of loss.
//
// The size goes on the long edge, not Immich's short edge: clip-probe measured
// that closer to the stored vectors (its ARCHITECTURE.md §6), and an upright
// JPEG within 1440 px is passed through its normalizer untouched instead of
// being resampled a second time.
//
// ponytail: an Adobe RGB or ProPhoto source keeps its profile, where Immich
// converts it to P3; add -profile with a P3 ICC file if such files drift.
var magickArgs = []string{
	"-auto-orient",
	"-colorspace", "RGB", // linear light: P3 shares sRGB's transfer curve, so this is exact for both
	"-filter", "Lanczos",
	"-resize", "1440x1440>", // long edge, never enlarge
	"-colorspace", "sRGB",
	"-quality", "80",
	"-sampling-factor", "4:4:4",
	"jpg:-",
}

// FindMagick resolves the magick setting: "off" disables ImageMagick, a path is
// taken as given, and empty searches PATH, then (on Windows) the default
// install folder. Returns "" when there is none.
func FindMagick(setting string) string {
	switch setting {
	case "off":
		return ""
	case "":
	default:
		return setting
	}
	if p, err := exec.LookPath("magick"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		// ponytail: lexical, so 7.1.10 sorts before 7.1.9; only matters with two installs.
		found, _ := filepath.Glob(filepath.Join(os.Getenv("ProgramFiles"), "ImageMagick-*", "magick.exe"))
		slices.Sort(found)
		if len(found) > 0 {
			return found[len(found)-1]
		}
		return "" // convert.exe on Windows is the disk tool, never ImageMagick
	}
	// ImageMagick 6, which Debian and Ubuntu still ship, has no magick binary.
	if p, err := exec.LookPath("convert"); err == nil {
		return p
	}
	return ""
}

// Render returns path rendered by magickArgs as JPEG bytes. Only the first
// frame of an animation or multi-page file is used, as Immich does.
func Render(ctx context.Context, magick, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, magick, append([]string{path + "[0]"}, magickArgs...)...)
	hideWindow(cmd)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg, _, _ := strings.Cut(strings.TrimSpace(errOut.String()), "\n")
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("imagemagick: %s", msg)
	}
	return out.Bytes(), nil
}
