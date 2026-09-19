package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// searchSQL mirrors DuplicateRepository.search, minus the self-exclusion (our
// query image is not an asset).
//
// The CTE is load-bearing. The distance filter must be applied AFTER the ordered
// limit: move `distance <= $5` into the inner WHERE and the planner can no longer
// satisfy the query from the ANN index, turning this into a sequential scan over
// every embedding in the library. Immich wraps it the same way for the same
// reason. `<=>` is cosine distance and must match the index's vector_cosine_ops.
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

	// Immich applies `ALTER DATABASE ... SET vchordrq.probes = 1`, so this is
	// usually redundant; it matters when VCHORD_PROBES is raised for recall.
	// A two-part GUC name is always settable, so an unknown one is harmless on a
	// pgvector-HNSW deployment -- but never fail a connection over it.
	probes := cfg.vchordProbes
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET vchordrq.probes = %d", probes)); err != nil {
			slog.Debug("could not set vchordrq.probes", "err", err)
		}
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

	return &store{pool: pool, ownerIDs: cfg.ownerIDs}, nil
}

func (s *store) close() { s.pool.Close() }

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
