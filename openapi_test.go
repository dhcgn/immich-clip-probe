package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

func newRequest(method, path string) (*http.Request, error) {
	return httptest.NewRequest(method, path, nil), nil
}

// openapi.yaml is a published contract, not a description written once and left
// to rot. These tests fail the build when the Go wire types and the spec drift
// apart -- a field added on one side and forgotten on the other is the most
// common way that happens, and it is invisible until a client breaks.
//
// This is deliberately not code generation. See ARCHITECTURE.md section 8.

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatalf("openapi.yaml does not load: %v", err)
	}
	// Also catches a malformed spec, which is worth knowing before publishing it.
	if err := doc.Validate(t.Context()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI document: %v", err)
	}
	return doc
}

// schemaOf resolves a named component, optionally descending into nested
// properties: schemaOf(t, doc, "Match", "links").
func schemaOf(t *testing.T, doc *openapi3.T, name string, path ...string) *openapi3.Schema {
	t.Helper()
	ref, ok := doc.Components.Schemas[name]
	if !ok {
		t.Fatalf("spec has no schema %q", name)
	}
	s := ref.Value
	for _, p := range path {
		next, ok := s.Properties[p]
		if !ok {
			t.Fatalf("schema %q has no property %q", name, p)
		}
		s = next.Value
	}
	return s
}

// Every wire type, populated so that omitempty fields are present too.
func wireTypes(t *testing.T) []struct {
	schema string
	path   []string
	value  any
} {
	t.Helper()
	now := time.Date(2024, 7, 18, 14, 2, 11, 0, time.UTC)
	m := match{
		AssetID:          "8f2c1d94-3a77-4c0e-9d51-6b0f2a1e77bc",
		OriginalFileName: "IMG_4821.HEIC",
		LocalDateTime:    &now,
		FileCreatedAt:    &now,
		OwnerID:          "3d1b0c6e-5f2a-4a1d-8e77-91c4f0b2a5de",
		Type:             "IMAGE",
		Distance:         0.0031,
		Similarity:       0.9969,
		Links: &links{
			Web:       "https://immich.example/photos/8f2c1d94",
			Thumbnail: "https://immich.example/api/assets/8f2c1d94/thumbnail",
		},
	}
	q := queryInfo{
		FileName: "IMG_4821.jpg", Bytes: 284193, Width: 1440, Height: 1080,
		Normalized: false, Model: "ViT-B-32__openai", Dimensions: 512,
	}
	tm := timings{NormalizeMs: 0, EmbedMs: 143, QueryMs: 8, TotalMs: 154}

	return []struct {
		schema string
		path   []string
		value  any
	}{
		{"SimilarResponse", nil, similarResponse{
			Query: q, Duplicate: true, MaxDistance: 0.01, Matches: []match{m}, Timings: tm,
		}},
		{"Query", nil, q},
		{"Match", nil, m},
		{"Match", []string{"links"}, *m.Links},
		{"Timings", nil, tm},
		// Every field must be non-zero, or an omitempty one is simply absent and
		// the field-set comparison below cannot see it.
		{"Health", nil, healthResponse{
			Status: "error", DB: "ok", ML: "error", Model: "ViT-B-32__openai",
			Dimensions: 512, IndexedAssets: 48213,
			Message: "machine learning container: connection refused",
		}},
		{"Error", nil, errorResponse{Error: errUnsupportedType, Message: "not a decodable image"}},
	}
}

// The check that matters: the set of JSON fields a Go type emits must equal the
// set of properties the spec declares. Schema validation alone would not catch a
// field added in Go and forgotten in the spec.
func TestWireTypesMatchSpecFields(t *testing.T) {
	doc := loadSpec(t)

	for _, tc := range wireTypes(t) {
		name := tc.schema
		if len(tc.path) > 0 {
			name += "." + tc.path[0]
		}
		t.Run(name, func(t *testing.T) {
			schema := schemaOf(t, doc, tc.schema, tc.path...)

			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}

			for field := range got {
				if _, ok := schema.Properties[field]; !ok {
					t.Errorf("Go emits %q but the spec does not declare it; add it to openapi.yaml", field)
				}
			}
			for field := range schema.Properties {
				if _, ok := got[field]; !ok {
					t.Errorf("spec declares %q but Go never emits it; remove it from openapi.yaml", field)
				}
			}

			// Types, enums and required-ness, now that the field sets agree.
			if err := schema.VisitJSON(got); err != nil {
				t.Errorf("value does not satisfy the schema: %v", err)
			}
		})
	}
}

// Error codes are the part of an error response clients are told to match on, so
// an undocumented one is a broken promise.
func TestErrorCodesMatchSpecEnum(t *testing.T) {
	doc := loadSpec(t)
	enum := schemaOf(t, doc, "Error", "error").Enum

	documented := make([]string, 0, len(enum))
	for _, v := range enum {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("non-string value %v in the Error.error enum", v)
		}
		documented = append(documented, s)
	}

	for _, code := range allErrCodes {
		if !slices.Contains(documented, string(code)) {
			t.Errorf("handler can emit %q but openapi.yaml does not document it", code)
		}
	}
	for _, code := range documented {
		if !slices.Contains(allErrCodes, errCode(code)) {
			t.Errorf("openapi.yaml documents %q but no handler emits it", code)
		}
	}
}

// A route that exists but is undocumented is invisible to anyone reading the
// spec. net/http offers no way to enumerate a ServeMux, so this asserts the
// other direction: everything the spec promises is actually served.
func TestSpecPathsAreServed(t *testing.T) {
	doc := loadSpec(t)
	srv := newServer(&config{apiToken: "t"}, nil, nil)

	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			t.Run(method+" "+path, func(t *testing.T) {
				req, err := newRequest(method, path)
				if err != nil {
					t.Fatal(err)
				}
				if _, pattern := srv.mux.Handler(req); pattern == "" {
					t.Errorf("spec documents %s %s but no route is registered", method, path)
				}
			})
		}
	}
}
