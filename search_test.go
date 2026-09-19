package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests need a Postgres with a vector extension. The Immich image has one:
//
//	docker run --rm -d -p 55432:5432 -e POSTGRES_PASSWORD=pw -e POSTGRES_USER=pg \
//	  -e POSTGRES_DB=test --name clipprobe-test \
//	  ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0
//	TEST_DB_URL=postgresql://pg:pw@localhost:55432/test go test ./...
//
// Without TEST_DB_URL they skip, so the default `go test ./...` stays green.
const (
	testDims = 512
	testRows = 600 // enough that a sequential scan is not the cheapest plan
)

func testStore(t *testing.T) (*store, []string) {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("set TEST_DB_URL to run database tests (see the comment in search_test.go)")
	}

	ctx := t.Context()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	ownerA := "3d1b0c6e-5f2a-4a1d-8e77-91c4f0b2a5de"
	ownerB := "9c5e2f10-7b3d-4e6a-bf21-0a7d4c8e1b93"

	// Minimal stand-ins for the two Immich tables the query touches. Column names
	// and the index mirror smart-search.table.ts and asset.table.ts.
	for _, stmt := range []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`DROP TABLE IF EXISTS smart_search, asset`,
		`CREATE TABLE asset (
			id uuid PRIMARY KEY,
			"ownerId" uuid NOT NULL,
			"originalFileName" text NOT NULL,
			"localDateTime" timestamptz,
			"fileCreatedAt" timestamptz,
			"deletedAt" timestamptz,
			type text NOT NULL,
			visibility text NOT NULL
		)`,
		fmt.Sprintf(`CREATE TABLE smart_search (
			"assetId" uuid PRIMARY KEY REFERENCES asset(id) ON DELETE CASCADE,
			embedding vector(%d) NOT NULL
		)`, testDims),
		// A real Immich deployment builds clip_index with vchordrq, not hnsw --
		// VectorChord's build options are version-specific and brittle to pin here.
		// The assertion below matches on the index *name*, so it holds for either:
		// what is being guarded is that the CTE shape lets an ANN index serve the
		// ordered limit at all.
		`CREATE INDEX clip_index ON smart_search USING hnsw (embedding vector_cosine_ops)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(stmt, "\n", 2)[0], err)
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range testRows {
		owner := ownerA
		if i%2 == 1 {
			owner = ownerB
		}
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		if _, err := tx.Exec(ctx,
			`INSERT INTO asset (id, "ownerId", "originalFileName", "localDateTime", "fileCreatedAt", type, visibility)
			 VALUES ($1, $2, $3, now(), now(), 'IMAGE', 'timeline')`,
			id, owner, fmt.Sprintf("IMG_%04d.jpg", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO smart_search ("assetId", embedding) VALUES ($1, $2::vector)`,
			id, randomVector()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE smart_search, asset`); err != nil {
		t.Fatal(err)
	}

	return &store{pool: pool, ownerIDs: []string{ownerA}}, []string{ownerA, ownerB}
}

func randomVector() string {
	var b strings.Builder
	b.WriteByte('[')
	for i := range testDims {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%.6f", rand.NormFloat64())
	}
	b.WriteByte(']')
	return b.String()
}

// The security-relevant one: a search must never return an asset belonging to an
// owner outside OWNER_IDS, however close its embedding happens to be.
func TestSearchScopesToConfiguredOwners(t *testing.T) {
	st, owners := testStore(t)
	ownerA, ownerB := owners[0], owners[1]

	// Without this the isolation assertion below could pass vacuously, on a table
	// that simply has no rows for the other owner.
	var otherRows int
	if err := st.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM smart_search ss
		 JOIN asset a ON a.id = ss."assetId" WHERE a."ownerId" = $1`, ownerB).Scan(&otherRows); err != nil {
		t.Fatal(err)
	}
	if otherRows == 0 {
		t.Fatal("fixture has no rows for the second owner; the test would prove nothing")
	}

	matches, err := st.search(t.Context(), randomVector(), []string{"IMAGE"}, 50, maxCosineDistance)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 50 {
		t.Fatalf("got %d matches, want the full limit of 50", len(matches))
	}
	for _, m := range matches {
		if m.OwnerID != ownerA {
			t.Fatalf("match %s belongs to %s, want only %s", m.AssetID, m.OwnerID, ownerA)
		}
	}
}

// The CTE shape exists so the ANN index can serve the ordered limit. Moving the
// distance filter inside the CTE silently turns this into a sequential scan over
// every embedding in the library -- slow, and invisible until someone notices.
func TestSearchUsesTheVectorIndex(t *testing.T) {
	st, _ := testStore(t)

	rows, err := st.pool.Query(t.Context(),
		`EXPLAIN `+searchSQL, randomVector(), st.ownerIDs, []string{"IMAGE"}, 10, maxCosineDistance)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(plan.String(), "clip_index") {
		t.Errorf("query plan does not use clip_index:\n%s", plan.String())
	}
}

// Threshold filtering happens outside the CTE, so a tight max_distance returns
// fewer rows without changing the plan.
func TestSearchAppliesThreshold(t *testing.T) {
	st, _ := testStore(t)
	probe := randomVector()

	all, err := st.search(t.Context(), probe, []string{"IMAGE"}, 25, maxCosineDistance)
	if err != nil {
		t.Fatal(err)
	}
	tight, err := st.search(t.Context(), probe, []string{"IMAGE"}, 25, 0.01)
	if err != nil {
		t.Fatal(err)
	}
	if len(tight) > len(all) {
		t.Errorf("threshold returned more rows (%d) than unfiltered (%d)", len(tight), len(all))
	}
	for _, m := range tight {
		if m.Distance > 0.01 {
			t.Errorf("distance %f exceeds the threshold", m.Distance)
		}
	}
	for _, m := range all {
		if m.Similarity != 1-m.Distance {
			t.Errorf("similarity %f is not 1-distance for %f", m.Similarity, m.Distance)
		}
	}
}

func TestDimsReportsStoredWidth(t *testing.T) {
	st, _ := testStore(t)
	got, err := st.dims(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != testDims {
		t.Errorf("dims = %d, want %d", got, testDims)
	}
}
