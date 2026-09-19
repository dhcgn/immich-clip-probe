package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type server struct {
	cfg   *config
	store *store
	ml    *mlClient
	mux   *http.ServeMux

	// storedDims caches vector_dims(embedding) from smart_search. 0 means
	// unknown (not yet read, or the table is empty), which disables the check.
	storedDims atomic.Int64
}

func newServer(cfg *config, st *store, ml *mlClient) *server {
	s := &server{cfg: cfg, store: st, ml: ml, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /v1/similar", s.handleSimilar)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// ---------------------------------------------------------------------------
// Wire types. Field rationale is in ARCHITECTURE.md section 8.
// ---------------------------------------------------------------------------

type similarResponse struct {
	Query       queryInfo `json:"query"`
	Duplicate   bool      `json:"duplicate"`
	MaxDistance float64   `json:"maxDistance"`
	Matches     []match   `json:"matches"`
	Timings     timings   `json:"timings"`
}

type queryInfo struct {
	FileName   string `json:"fileName,omitempty"`
	Bytes      int    `json:"bytes"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Normalized bool   `json:"normalized"`
	Model      string `json:"model"`
	Dimensions int    `json:"dimensions"`
}

type match struct {
	AssetID          string     `json:"assetId"`
	OriginalFileName string     `json:"originalFileName"`
	LocalDateTime    *time.Time `json:"localDateTime,omitempty"`
	FileCreatedAt    *time.Time `json:"fileCreatedAt,omitempty"`
	OwnerID          string     `json:"ownerId"`
	Type             string     `json:"type"`
	Distance         float64    `json:"distance"`
	Similarity       float64    `json:"similarity"`
	Links            *links     `json:"links,omitempty"`
}

type links struct {
	Web       string `json:"web"`
	Thumbnail string `json:"thumbnail"`
}

type timings struct {
	NormalizeMs int64 `json:"normalizeMs"`
	EmbedMs     int64 `json:"embedMs"`
	QueryMs     int64 `json:"queryMs"`
	TotalMs     int64 `json:"totalMs"`
}

type healthResponse struct {
	Status        string `json:"status"`
	DB            string `json:"db"`
	ML            string `json:"ml"`
	Model         string `json:"model"`
	Dimensions    int    `json:"dimensions,omitempty"`
	IndexedAssets int64  `json:"indexedAssets"`
	Message       string `json:"message,omitempty"`
}

// errCode is the stable, machine-readable half of an error response. Clients
// match on this; the message is free text and not a contract.
//
// These values are duplicated in openapi.yaml as the Error.error enum, and
// openapi_test.go asserts the two sets are identical.
type errCode string

const (
	errUnauthorized    errCode = "unauthorized"
	errBadRequest      errCode = "bad_request"
	errPayloadTooLarge errCode = "payload_too_large"
	errUnsupportedType errCode = "unsupported_media"
	errZeroSizeImage   errCode = "zero_size_image"
	errMLUnavailable   errCode = "ml_unavailable"
	errModelMismatch   errCode = "model_mismatch"
	errDBUnavailable   errCode = "db_unavailable"
)

// allErrCodes is what the contract test checks against the spec.
var allErrCodes = []errCode{
	errUnauthorized, errBadRequest, errPayloadTooLarge, errUnsupportedType,
	errZeroSizeImage, errMLUnavailable, errModelMismatch, errDBUnavailable,
}

type errorResponse struct {
	Error   errCode `json:"error"`
	Message string  `json:"message"`
}

// ---------------------------------------------------------------------------

func (s *server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, errUnauthorized, "missing or invalid x-api-key")
		return
	}

	limit, maxDist, all, types, err := s.params(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, errBadRequest, err.Error())
		return
	}

	raw, fileName, err := s.readImage(w, r)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, http.StatusRequestEntityTooLarge, errPayloadTooLarge,
				"body exceeds MAX_UPLOAD_BYTES ("+strconv.FormatInt(s.cfg.maxUploadBytes, 10)+" bytes)")
			return
		}
		writeError(w, http.StatusBadRequest, errBadRequest, err.Error())
		return
	}

	t0 := time.Now()
	img, width, height, normalized, err := normalize(raw, s.cfg.previewParity)
	normalizeMs := time.Since(t0).Milliseconds()
	if err != nil {
		if errors.Is(err, errUndecodable) {
			writeError(w, http.StatusUnsupportedMediaType, errUnsupportedType,
				"not a decodable image; HEIC and camera RAW are not supported, convert to JPEG first")
			return
		}
		writeError(w, http.StatusUnsupportedMediaType, errZeroSizeImage, err.Error())
		return
	}

	t0 = time.Now()
	embedding, err := s.ml.encode(r.Context(), img)
	embedMs := time.Since(t0).Milliseconds()
	if err != nil {
		slog.Warn("encode failed", "err", err)
		writeError(w, http.StatusBadGateway, errMLUnavailable, "machine learning container: "+err.Error())
		return
	}

	dims := vectorDims(embedding)
	if stored := int(s.storedDims.Load()); stored > 0 && dims != stored {
		slog.Error("model mismatch", "configured", s.cfg.clipModel, "got_dims", dims, "stored_dims", stored)
		writeError(w, http.StatusInternalServerError, errModelMismatch,
			"CLIP_MODEL "+s.cfg.clipModel+" yields "+strconv.Itoa(dims)+
				" dimensions but smart_search stores "+strconv.Itoa(stored)+
				"; set CLIP_MODEL to the model Immich indexed with")
		return
	}

	threshold := maxDist
	if all {
		threshold = maxCosineDistance
	}

	t0 = time.Now()
	matches, err := s.store.search(r.Context(), embedding, types, limit, threshold)
	queryMs := time.Since(t0).Milliseconds()
	if err != nil {
		slog.Error("search failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, errDBUnavailable, "database: "+err.Error())
		return
	}
	for i := range matches {
		matches[i].Links = s.linksFor(matches[i].AssetID)
	}

	writeJSON(w, http.StatusOK, similarResponse{
		Query: queryInfo{
			FileName:   fileName,
			Bytes:      len(raw),
			Width:      width,
			Height:     height,
			Normalized: normalized,
			Model:      s.cfg.clipModel,
			Dimensions: dims,
		},
		// With ?all=true no threshold was applied, so the question was not asked.
		Duplicate:   !all && len(matches) > 0 && matches[0].Distance <= maxDist,
		MaxDistance: maxDist,
		Matches:     matches,
		Timings: timings{
			NormalizeMs: normalizeMs,
			EmbedMs:     embedMs,
			QueryMs:     queryMs,
			TotalMs:     time.Since(start).Milliseconds(),
		},
	})
}

// handleHealth is unauthenticated on purpose: Docker and reverse proxies need it.
// It therefore reports no asset data beyond a count.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	out := healthResponse{Status: "ok", DB: "ok", ML: "ok", Model: s.cfg.clipModel}

	if err := s.ml.ping(ctx); err != nil {
		out.Status, out.ML, out.Message = "error", "error", "machine learning container: "+err.Error()
	}

	dims, err := s.store.dims(ctx)
	if err != nil {
		out.Status, out.DB = "error", "error"
		out.Message = "database: " + err.Error()
	} else {
		out.Dimensions = dims
		s.storedDims.Store(int64(dims))
		if n, err := s.store.indexedAssets(ctx); err == nil {
			out.IndexedAssets = n
		}
	}

	// Only meaningful once both legs answered.
	if out.Status == "ok" && out.Dimensions > 0 {
		if err := s.checkModelDims(ctx, out.Dimensions); err != nil {
			out.Status, out.Message = "error", err.Error()
		}
	}

	status := http.StatusOK
	if out.Status != "ok" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, out)
}

// checkModelDims embeds a tiny probe image and compares the result against what
// smart_search stores. This is the check that stops a mismatched CLIP_MODEL from
// silently returning nonsense neighbours.
func (s *server) checkModelDims(ctx context.Context, stored int) error {
	embedding, err := s.ml.encode(ctx, probeJPEG)
	if err != nil {
		return errors.New("probe embedding failed: " + err.Error())
	}
	if got := vectorDims(embedding); got != stored {
		return errors.New("model mismatch: CLIP_MODEL " + s.cfg.clipModel +
			" yields " + strconv.Itoa(got) + " dims, smart_search stores " + strconv.Itoa(stored))
	}
	return nil
}

// refreshDims primes the cached dimension at startup so the very first request
// is already protected. Best effort: the ML container may still be loading.
func (s *server) refreshDims(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	dims, err := s.store.dims(ctx)
	if err != nil {
		slog.Warn("preflight: could not read smart_search dimensions", "err", err)
		return
	}
	s.storedDims.Store(int64(dims))
	if dims == 0 {
		slog.Warn("preflight: smart_search is empty; has Immich's Smart Search job run?")
		return
	}
	if err := s.checkModelDims(ctx, dims); err != nil {
		slog.Error("preflight: " + err.Error())
		return
	}
	slog.Info("preflight ok", "model", s.cfg.clipModel, "dimensions", dims)
}

// ---------------------------------------------------------------------------

func (s *server) authorized(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("x-api-key")), []byte(s.cfg.apiToken)) == 1
}

func (s *server) params(r *http.Request) (limit int, maxDist float64, all bool, types []string, err error) {
	q := r.URL.Query()

	limit = s.cfg.defaultLimit
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 100 {
			return 0, 0, false, nil, errors.New("limit must be an integer between 1 and 100")
		}
	}

	maxDist = s.cfg.maxDistance
	if v := q.Get("max_distance"); v != "" {
		if maxDist, err = strconv.ParseFloat(v, 64); err != nil || maxDist < 0 || maxDist > maxCosineDistance {
			return 0, 0, false, nil, errors.New("max_distance must be a number between 0 and 2")
		}
	}

	if v := q.Get("all"); v != "" {
		if all, err = strconv.ParseBool(v); err != nil {
			return 0, 0, false, nil, errors.New("all must be true or false")
		}
	}

	switch strings.ToUpper(q.Get("type")) {
	case "", "IMAGE":
		types = []string{"IMAGE"}
	case "VIDEO":
		types = []string{"VIDEO"}
	case "ALL":
		types = []string{"IMAGE", "VIDEO"}
	default:
		return 0, 0, false, nil, errors.New("type must be IMAGE, VIDEO or all")
	}

	return limit, maxDist, all, types, nil
}

// readImage accepts either a multipart `file` part or the raw bytes as the body.
// The size limit is applied before the body is read, not after.
func (s *server) readImage(w http.ResponseWriter, r *http.Request) (raw []byte, fileName string, err error) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.maxUploadBytes)

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		file, header, err := r.FormFile("file")
		if err != nil {
			return nil, "", errors.New(`multipart request without a "file" part`)
		}
		defer file.Close()
		raw, err = io.ReadAll(file)
		if err != nil {
			return nil, "", err
		}
		return raw, header.Filename, nil
	}

	raw, err = io.ReadAll(r.Body)
	if err != nil {
		return nil, "", err
	}
	if len(raw) == 0 {
		return nil, "", errors.New("empty request body")
	}
	return raw, "", nil
}

func (s *server) linksFor(assetID string) *links {
	if s.cfg.publicImmichURL == "" {
		return nil
	}
	return &links{
		Web:       s.cfg.publicImmichURL + "/photos/" + assetID,
		Thumbnail: s.cfg.publicImmichURL + "/api/assets/" + assetID + "/thumbnail",
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("writing response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code errCode, message string) {
	writeJSON(w, status, errorResponse{Error: code, Message: message})
}
