package main

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/dhcgn/immich-clip-probe/tools/check"
)

// Thumbnails come from the app's own asset server under /thumb/, so the page
// can use plain lazy-loading <img> tags.

// What the webview decodes itself, for when there is no ImageMagick.
var browserExts = map[string]bool{".jpg": true, ".jpeg": true, ".jpe": true, ".jfif": true, ".png": true, ".gif": true, ".webp": true, ".avif": true, ".bmp": true}

// thumbSlots caps the magick processes thumbnails start next to the check's own.
var thumbSlots = make(chan struct{}, 4)

func (s *CheckService) handler(assets http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /thumb/local", s.localThumb)
	mux.HandleFunc("GET /thumb/asset/{id}", s.assetThumb)
	mux.Handle("/", assets)
	return mux
}

// localThumb shows a file the user picked for a check, and no other path.
func (s *CheckService) localThumb(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	s.mu.Lock()
	ok := s.checked[path]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	cfg, _ := settings()
	if magick := check.FindMagick(cfg.Magick); magick != "" {
		thumbSlots <- struct{}{}
		img, err := check.Thumbnail(r.Context(), magick, path)
		<-thumbSlots
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(img)
		return
	}
	if browserExts[strings.ToLower(filepath.Ext(path))] {
		http.ServeFile(w, r, path) // the webview scales it and applies EXIF orientation
		return
	}
	http.NotFound(w, r)
}

// assetThumb fetches a match's thumbnail from Immich with the API key from
// the settings, which the webview could not send itself.
func (s *CheckService) assetThumb(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg, err := settings()
	if err != nil || cfg.ImmichURL == "" || cfg.ImmichKey == "" || len(id) != 36 || strings.Trim(id, "0123456789abcdef-") != "" {
		http.NotFound(w, r)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(cfg.ImmichURL, "/")+"/api/assets/"+id+"/thumbnail", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Header.Set("x-api-key", cfg.ImmichKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
