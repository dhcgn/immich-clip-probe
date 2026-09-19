// Command clip-probe exposes Immich's CLIP similarity search for files that are
// not in Immich yet: it encodes a submitted image with Immich's own machine
// learning container, then runs the same cosine nearest-neighbour query Immich's
// duplicate detector runs against the smart_search table.
//
// See ARCHITECTURE.md for the design and for source references backing every
// contract this program depends on.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is set at build time via -ldflags. Logged at startup because the first
// question when compatibility breaks is "which build is running".
var version = "dev"

type config struct {
	listenAddr      string
	apiToken        string
	ownerIDs        []string
	dbURL           string
	mlURL           string
	publicImmichURL string
	clipModel       string
	maxDistance     float64
	defaultLimit    int
	maxUploadBytes  int64
	previewParity   bool
	vchordProbes    int
	logLevel        slog.Level
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		// slog's default handler is fine here; the configured one needs cfg.
		slog.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.logLevel})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := newStore(ctx, cfg)
	if err != nil {
		slog.Error("database", "err", err)
		os.Exit(1)
	}
	defer store.close()

	srv := newServer(cfg, store, newMLClient(cfg))

	// Best effort: report the model/dimension situation at startup rather than
	// leaving it to the first request. Never fatal -- the ML container may still
	// be downloading a model, and restart:always plus a failing healthcheck is
	// the better way to express that.
	go srv.refreshDims(ctx)

	h := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      2 * time.Minute, // a cold ML container reloads its model
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", cfg.listenAddr, "version", version,
			"model", cfg.clipModel, "owners", len(cfg.ownerIDs), "previewParity", cfg.previewParity)
		if err := h.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.Shutdown(shutdownCtx); err != nil {
		slog.Warn("shutdown", "err", err)
	}
}

func loadConfig() (*config, error) {
	c := &config{
		listenAddr:      env("LISTEN_ADDR", ":8080"),
		apiToken:        os.Getenv("API_TOKEN"),
		dbURL:           os.Getenv("DB_URL"),
		mlURL:           strings.TrimRight(env("IMMICH_ML_URL", "http://immich-machine-learning:3003"), "/"),
		publicImmichURL: strings.TrimRight(os.Getenv("PUBLIC_IMMICH_URL"), "/"),
		clipModel:       env("CLIP_MODEL", "ViT-B-32__openai"),
	}

	if c.apiToken == "" {
		return nil, errors.New("API_TOKEN is required (openssl rand -hex 32)")
	}
	if c.dbURL == "" {
		return nil, errors.New("DB_URL is required")
	}

	for id := range strings.SplitSeq(os.Getenv("OWNER_IDS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			if len(id) != 36 {
				return nil, fmt.Errorf("OWNER_IDS: %q is not a UUID", id)
			}
			c.ownerIDs = append(c.ownerIDs, id)
		}
	}
	if len(c.ownerIDs) == 0 {
		return nil, errors.New(`OWNER_IDS is required; find it with: SELECT id, email FROM "user"`)
	}

	var err error
	if c.maxDistance, err = envFloat("MAX_DISTANCE", 0.01); err != nil {
		return nil, err
	}
	if c.defaultLimit, err = envInt("DEFAULT_LIMIT", 10); err != nil {
		return nil, err
	}
	if c.vchordProbes, err = envInt("VCHORD_PROBES", 1); err != nil {
		return nil, err
	}
	maxUpload, err := envInt("MAX_UPLOAD_BYTES", 50<<20)
	if err != nil {
		return nil, err
	}
	c.maxUploadBytes = int64(maxUpload)
	c.previewParity = env("PREVIEW_PARITY", "true") != "false"

	switch strings.ToLower(env("LOG_LEVEL", "info")) {
	case "debug":
		c.logLevel = slog.LevelDebug
	case "warn", "warning":
		c.logLevel = slog.LevelWarn
	case "error":
		c.logLevel = slog.LevelError
	default:
		c.logLevel = slog.LevelInfo
	}

	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}
