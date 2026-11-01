package check

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The wire types mirror what this client reads of handler.go's similarResponse,
// match and healthResponse; openapi.yaml is the contract.

// Match is one Immich asset near the query.
type Match struct {
	AssetID          string     `json:"assetId"`
	OriginalFileName string     `json:"originalFileName"`
	LocalDateTime    *time.Time `json:"localDateTime,omitempty"`
	Type             string     `json:"type"`
	Distance         float64    `json:"distance"`
	Links            *Links     `json:"links,omitempty"`
}

// Links are only present when the server has PUBLIC_IMMICH_URL set.
type Links struct {
	Web string `json:"web"`
}

// Health is GET /healthz.
type Health struct {
	Status        string `json:"status"`
	DB            string `json:"db"`
	ML            string `json:"ml"`
	Model         string `json:"model"`
	Dimensions    int    `json:"dimensions,omitempty"`
	IndexedAssets int64  `json:"indexedAssets"`
	Message       string `json:"message,omitempty"`
}

// APIError is the server's {"error": code, "message": text}. Match on Code.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

type Client struct {
	Server string
	Token  string
	HTTP   *http.Client
}

func NewClient(server, token string) *Client {
	// Generous: the first request after the ML container evicted its model pays a reload.
	return &Client{Server: strings.TrimRight(server, "/"), Token: token, HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

// Similar returns the limit nearest assets to img, closest first.
//
// It asks with all=true and never sends max_distance: the nearest distance
// always comes back, and the caller decides what counts as a match. With a
// threshold on the wire, everything beyond it comes back empty and the distance
// is lost.
func (c *Client) Similar(ctx context.Context, name string, img []byte, limit int) ([]Match, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(img); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	q := url.Values{"all": {"true"}, "limit": {strconv.Itoa(max(limit, 1))}, "type": {"IMAGE"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Server+"/v1/similar?"+q.Encode(), &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("x-api-key", c.Token)

	var out struct {
		Matches []Match `json:"matches"`
	}
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out.Matches, nil
}

// Health reports the server's state. A 503 still carries a Health body saying
// which leg is down, so only an unreadable answer is an error.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Server+"/healthz", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var h Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, fmt.Errorf("healthz returned %d and no health report: %w", resp.StatusCode, err)
	}
	return &h, nil
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		e := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(raw, e) != nil || e.Code == "" {
			e.Code, e.Message = "http_"+strconv.Itoa(resp.StatusCode), strings.TrimSpace(string(raw))
		}
		return e
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
