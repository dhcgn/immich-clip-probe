package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/dhcgn/immich-clip-probe/tools/check"
)

// The optional Immich connection: thumbnails of the matches, and whose key
// the status bar is using.

// ImmichInfo is the Immich half of the status bar. Nil when no Immich is set.
type ImmichInfo struct {
	Key   string `json:"key,omitempty"`   // the API key's name in Immich
	Owner string `json:"owner,omitempty"` // "Name <email>", if the key may read its user
	Error string `json:"error,omitempty"`
}

// immichGet calls the Immich API with the key from the settings.
func immichGet(ctx context.Context, cfg check.Config, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.ImmichURL, "/")+"/api/"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", cfg.ImmichKey)
	return http.DefaultClient.Do(req)
}

func immichJSON(ctx context.Context, cfg check.Config, path string, out any) error {
	resp, err := immichGet(ctx, cfg, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Immich answered %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// immichStatus checks the key and whether it can fetch thumbnails.
// api-keys/me answers any valid key; the owner comes from users/me, which
// needs user.read, so a key scoped to asset.view still counts as working.
func immichStatus(ctx context.Context, cfg check.Config) *ImmichInfo {
	var key struct {
		Name        string   `json:"name"`
		Permissions []string `json:"permissions"`
	}
	if err := immichJSON(ctx, cfg, "api-keys/me", &key); err != nil {
		return &ImmichInfo{Error: err.Error()}
	}
	info := &ImmichInfo{Key: key.Name}
	if !slices.Contains(key.Permissions, "all") && !slices.Contains(key.Permissions, "asset.view") {
		info.Error = "the key lacks the asset.view permission, so thumbnails cannot load"
	}
	var me struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if immichJSON(ctx, cfg, "users/me", &me) == nil {
		info.Owner = me.Name + " <" + me.Email + ">"
	}
	return info
}
