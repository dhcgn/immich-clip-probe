package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dhcgn/immich-clip-probe/tools/check"
)

// fakeImmich answers api-keys/me with perms (or 401 for a bad key) and
// users/me only when the key may read users.
func fakeImmich(t *testing.T, perms string) check.Config {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("x-api-key") != "good":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/api/api-keys/me":
			io.WriteString(w, `{"name":"clip-check","permissions":`+perms+`}`)
		case r.URL.Path == "/api/users/me" && strings.Contains(perms, `"all"`):
			io.WriteString(w, `{"name":"Daniel","email":"d@example.com"}`)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(srv.Close)
	return check.Config{ImmichURL: srv.URL + "/", ImmichKey: "good"}
}

func TestImmichStatus(t *testing.T) {
	ctx := context.Background()
	if got := immichStatus(ctx, fakeImmich(t, `["all"]`)); got.Error != "" || got.Owner != "Daniel <d@example.com>" {
		t.Errorf("full key: %+v, want the owner and no error", got)
	}
	if got := immichStatus(ctx, fakeImmich(t, `["asset.view"]`)); got.Error != "" || got.Owner != "" || got.Key != "clip-check" {
		t.Errorf("asset.view key: %+v, want working, named by the key since users/me is forbidden", got)
	}
	if got := immichStatus(ctx, fakeImmich(t, `["album.read"]`)); !strings.Contains(got.Error, "asset.view") {
		t.Errorf("key without asset.view: %+v, want an error naming the permission", got)
	}
	cfg := fakeImmich(t, `["all"]`)
	cfg.ImmichKey = "wrong"
	if got := immichStatus(ctx, cfg); !strings.Contains(got.Error, "401") {
		t.Errorf("bad key: %+v, want the 401", got)
	}
}
