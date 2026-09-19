package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// mlClient talks to immich-machine-learning. There is no authentication on that
// container and none is available -- see ARCHITECTURE.md section 4.
type mlClient struct {
	url    string
	model  string
	client *http.Client
}

func newMLClient(cfg *config) *mlClient {
	return &mlClient{
		url:   cfg.mlURL,
		model: cfg.clipModel,
		// Generous: the first request after a model eviction pays a reload.
		client: &http.Client{Timeout: 90 * time.Second},
	}
}

// encode returns the CLIP embedding of img as Immich's ML container produces it:
// a JSON array rendered as a string, e.g. "[0.013,-0.041,...]".
//
// That string is deliberately NOT parsed. Immich's serialize_np_array returns
// raw JSON text precisely so callers can pass it through, and the format is
// already a valid pgvector literal, so it goes straight into the query. Parsing
// and re-formatting would only add float round-trip error.
func (c *mlClient) encode(ctx context.Context, img []byte) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	// Matches MachineLearningRepository.encodeImage:
	//   { [ModelTask.SEARCH]: { [ModelType.VISUAL]: { modelName } } }
	// with SEARCH = "clip" and VISUAL = "visual".
	entries, err := json.Marshal(map[string]map[string]map[string]string{
		"clip": {"visual": {"modelName": c.model}},
	})
	if err != nil {
		return "", err
	}
	if err := w.WriteField("entries", string(entries)); err != nil {
		return "", err
	}
	part, err := w.CreateFormFile("image", "image")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(img); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/predict", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("predict returned %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}

	var out struct {
		Clip string `json:"clip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding predict response: %w", err)
	}
	if out.Clip == "" {
		return "", fmt.Errorf("predict returned no %q field; the ML contract may have changed (see .claude/skills/immich-compat)", "clip")
	}
	return out.Clip, nil
}

// ping reports whether the ML container is reachable.
func (c *mlClient) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/ping", nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := c.client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ping returned %d", resp.StatusCode)
	}
	return nil
}

// vectorDims counts the elements in an embedding literal without parsing the
// floats. Used only to compare against vector_dims(embedding) from smart_search.
func vectorDims(embedding string) int {
	if len(embedding) < 3 { // "[]" or shorter carries no values
		return 0
	}
	return strings.Count(embedding, ",") + 1
}
