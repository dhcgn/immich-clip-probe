package main

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// searchSQL mirrors DuplicateRepository.search, minus the self-exclusion (our
// query image is not an asset).
//
// The CTE keeps the inner query a plain ORDER BY ... LIMIT, which is the form an
// ANN index serves, and it is the shape Immich itself uses. Moving `distance <=
// $5` into the inner WHERE returns the same rows -- the difference is what the
// planner can do with it, not the result -- so nothing but an EXPLAIN at
// production scale will tell you it regressed. Leave the shape alone.
//
// `<=>` is cosine distance and must match the index's vector_cosine_ops.
//
// See ARCHITECTURE.md section 5.
const searchSQL = `
WITH cte AS (
  SELECT a.id,
         a."originalFileName",
         a."localDateTime",
         a."fileCreatedAt",
         a."ownerId",
         a.type::text AS type,
         ss.embedding <=> $1::vector AS distance
  FROM smart_search ss
  INNER JOIN asset a ON a.id = ss."assetId"
  WHERE a."ownerId"   = ANY($2::uuid[])
    AND a."deletedAt" IS NULL
    AND a.visibility IN ('timeline', 'archive')
    AND a.type::text  = ANY($3::text[])
  ORDER BY distance
  LIMIT $4
)
SELECT * FROM cte WHERE distance <= $5`

// maxCosineDistance is the largest value `<=>` can return, so passing it as the
// threshold makes the outer filter a no-op. That keeps ?all=true on one SQL
// string instead of two.
const maxCosineDistance = 2.0

type store struct {
	pool     *pgxpool.Pool
	ownerIDs []string
}

func newStore(ctx context.Context, cfg *config) (*store, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.dbURL)
	if err != nil {
		return nil, fmt.Errorf("DB_URL: %w", err)
	}

	// The database default Immich applies (`vchordrq.probes = 1`) is not what
	// Immich searches with: every smart search first issues `SET LOCAL
	// vchordrq.probes` with probesFor(clip_index). Inheriting the default instead
	// scans one list of up to hundreds, and the true match is silently missed.
	// Read per connection, so an index Immich rebuilds with more lists is picked
	// up when the pool recycles connections (pgxpool: hourly).
	//
	// A two-part GUC name is always settable, so an unknown one is harmless on a
	// pgvector-HNSW deployment -- but never fail a connection over it.
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		probes := cfg.vchordProbes
		if probes <= 0 {
			// No row (no index yet) leaves def empty, which probesFor reads as one list.
			var def string
			if err := conn.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'clip_index'`).Scan(&def); err != nil && err != pgx.ErrNoRows {
				slog.Debug("could not read clip_index definition", "err", err)
			}
			probes = probesFor(def)
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET vchordrq.probes = %d", probes)); err != nil {
			slog.Debug("could not set vchordrq.probes", "err", err)
		}
		slog.Debug("connected", "vchordrq.probes", probes)
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, err
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, err
	}
	// What the session actually got: the first thing to check when recall is off.
	var probes string
	if err := pool.QueryRow(pingCtx, `SHOW vchordrq.probes`).Scan(&probes); err == nil {
		slog.Info("vector search", "vchordrq.probes", probes)
	}

	return &store{pool: pool, ownerIDs: cfg.ownerIDs}, nil
}

func (s *store) close() { s.pool.Close() }

// listsRe finds the IVF list count in a vchordrq index definition; it is the
// same pattern Immich's reindexVectorsIfNeeded uses.
var listsRe = regexp.MustCompile(`lists = \[(\d+)`)

// probesFor mirrors Immich's targetProbeCount: probe an eighth of the lists.
// A definition without lists (HNSW, or no index) counts as one list, as in
// Immich.
func probesFor(indexdef string) int {
	m := listsRe.FindStringSubmatch(indexdef)
	if m == nil {
		return 1
	}
	lists, _ := strconv.Atoi(m[1])
	return max(1, (lists+7)/8)
}

// search returns the assets nearest to embedding, ascending by cosine distance.
// Pass maxCosineDistance as maxDist to disable the threshold.
func (s *store) search(ctx context.Context, embedding string, types []string, limit int, maxDist float64) ([]match, error) {
	rows, err := s.pool.Query(ctx, searchSQL, embedding, s.ownerIDs, types, limit, maxDist)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	matches := []match{} // never nil: the JSON field must be [] rather than null
	for rows.Next() {
		var m match
		if err := rows.Scan(
			&m.AssetID,
			&m.OriginalFileName,
			&m.LocalDateTime,
			&m.FileCreatedAt,
			&m.OwnerID,
			&m.Type,
			&m.Distance,
		); err != nil {
			return nil, err
		}
		m.Similarity = 1 - m.Distance
		matches = append(matches, m)
	}
	return matches, rows.Err()
}

// dims reports the embedding width stored in smart_search, or 0 when the table
// is empty. Compared against the width the ML container returns, this is what
// catches a CLIP_MODEL that does not match what Immich indexed with.
func (s *store) dims(ctx context.Context) (int, error) {
	var d int
	err := s.pool.QueryRow(ctx, `SELECT vector_dims(embedding) FROM smart_search LIMIT 1`).Scan(&d)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	return d, err
}

// indexedAssets counts the searchable rows for the configured owners. Zero means
// Immich's Smart Search job has never run, which otherwise looks like "nothing
// ever matches".
func (s *store) indexedAssets(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM smart_search ss
		INNER JOIN asset a ON a.id = ss."assetId"
		WHERE a."ownerId" = ANY($1::uuid[]) AND a."deletedAt" IS NULL`,
		s.ownerIDs).Scan(&n)
	return n, err
}
