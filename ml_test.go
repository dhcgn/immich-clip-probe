package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The `entries` field is the contract with Immich's ML container. It is built
// from MachineLearningRepository.encodeImage, whose enum values are
// ModelTask.SEARCH = "clip" and ModelType.VISUAL = "visual". If this assertion
// ever fails against a new Immich release, see .claude/skills/immich-compat.
func TestEncodeSendsImmichsEntriesShape(t *testing.T) {
	const wantEntries = `{"clip":{"visual":{"modelName":"ViT-B-32__openai"}}}`
	const cannedClip = `[0.013,-0.041,0.5]`

	var gotEntries string
	var gotImage []byte
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("not a multipart request: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		gotEntries = r.FormValue("entries")
		file, _, err := r.FormFile("image")
		if err != nil {
			t.Errorf(`no "image" part: %v`, err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		defer file.Close()
		gotImage, _ = io.ReadAll(file)
		w.Write([]byte(`{"clip":"` + cannedClip + `","imageWidth":16,"imageHeight":16}`))
	}))
	defer srv.Close()

	c := newMLClient(&config{mlURL: srv.URL, clipModel: "ViT-B-32__openai"})
	got, err := c.encode(context.Background(), probeJPEG)
	if err != nil {
		t.Fatal(err)
	}

	if gotPath != "/predict" {
		t.Errorf("path = %q, want /predict", gotPath)
	}
	if gotEntries != wantEntries {
		t.Errorf("entries =\n  %s\nwant\n  %s", gotEntries, wantEntries)
	}
	if len(gotImage) != len(probeJPEG) {
		t.Errorf("image part = %d bytes, want %d", len(gotImage), len(probeJPEG))
	}

	// The embedding must reach the caller byte for byte: it is handed straight to
	// Postgres as a pgvector literal, and parsing it would introduce float
	// round-trip error for nothing.
	if got != cannedClip {
		t.Errorf("embedding = %q, want %q verbatim", got, cannedClip)
	}
}

// A response without a "clip" key means the ML contract moved. Fail loudly
// rather than sending an empty string to Postgres.
func TestEncodeRejectsMissingClipField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"imageWidth":16,"imageHeight":16}`))
	}))
	defer srv.Close()

	c := newMLClient(&config{mlURL: srv.URL, clipModel: "ViT-B-32__openai"})
	if _, err := c.encode(context.Background(), probeJPEG); err == nil {
		t.Fatal("want an error when the clip field is absent")
	}
}

func TestEncodeReportsUpstreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Invalid request format.", http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	c := newMLClient(&config{mlURL: srv.URL, clipModel: "ViT-B-32__openai"})
	_, err := c.encode(context.Background(), probeJPEG)
	if err == nil {
		t.Fatal("want an error for a 422 response")
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("err = %v, want it to mention the status code", err)
	}
}

func TestVectorDims(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{`[0.1,0.2,0.3]`, 3},
		{`[0.1]`, 1},
		{``, 0},
		{`[]`, 0},
	} {
		if got := vectorDims(tc.in); got != tc.want {
			t.Errorf("vectorDims(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
