# Architecture

Every claim here was read out of the Immich source tree. All links are permalinks
to commit [`202015e`][tree] on `main` (2026-09-19), which reports server version
`3.2.0`. That commit is **not** an ancestor of the `v3.2.x` release tags, so links
are pinned by SHA rather than by tag — line numbers will always resolve.

A full index of references is in [§13](#13-source-references).

---

## 1. The idea in one paragraph

Immich computes a CLIP embedding for every asset and stores it in a pgvector
column. Its duplicate detector then asks Postgres for the nearest neighbours of an
asset's embedding. Both halves of that machinery are reachable from inside the
Docker network — the ML container's [`/predict`][mlpredict] endpoint, and the
[`smart_search`][sstable] table — but Immich's public HTTP API never lets you feed
it an image that is not already an asset. This service is a thin adapter that
supplies the missing input: it takes an arbitrary file, gets an embedding for it
from the same ML container, and runs the same nearest-neighbour query. It is ~400
lines of Go and owns no state.

## 2. Context

```
                            ┌──────────────────────────────────┐
  you / a script            │        immich (compose)          │
        │                   │                                  │
        │ POST /v1/similar  │  ┌────────────────────────────┐  │
        │ x-api-key         │  │ immich-machine-learning    │  │
        ▼                   │  │  POST /predict  :3003      │  │
  ┌─────────────┐───────────┼─▶│                            │  │
  │ clip-probe  │◀──────────┼──└────────────────────────────┘  │  embedding
  │   :8080     │           │  ┌────────────────────────────┐  │
  │   (Go)      │───────────┼─▶│ immich_postgres :5432      │  │
  └─────────────┘           │  │  smart_search, asset       │  │  SELECT only
        │                   │  └────────────────────────────┘  │
        │                   │  ┌────────────────────────────┐  │
        │                   │  │ immich-server :2283        │  │  ── not used ──
        │                   │  └────────────────────────────┘  │
        └───────────────────┴──────────────────────────────────┘
          PUBLIC_IMMICH_URL is string-concatenated into links. Never fetched.
```

Two runtime dependencies, both on the internal network:

| Dependency | Used for | Coupling |
| --- | --- | --- |
| `immich-machine-learning` | encode the uploaded file to a vector | internal API, undocumented |
| `immich_postgres` | nearest-neighbour search | internal schema, undocumented |

`immich-server` is deliberately **not** a dependency. An earlier design validated
the caller's Immich API key against `GET /api/users/me` to derive the owner; the
chosen design uses a token of this service's own instead (see [§7](#7-security-model)),
which removes the third dependency, removes a network round trip from the hot
path, and keeps the service functional while `immich-server` is restarting.

Note the ML and database ports are **not published to the host** in the stock
Immich compose file. The service therefore has to be a container on
`immich_default` (or whatever your project's default network is). That is a
deployment constraint, not a design preference.

## 3. Request flow

```
POST /v1/similar   (image, x-api-key)
  │
  ├─ 1. auth       ── constant-time compare against API_TOKEN
  │
  ├─ 2. decode     ── image.Decode; reject if it is not a decodable image
  │
  ├─ 3. parity     ── already JPEG with long edge <= 1440?  pass through untouched
  │                   otherwise resize to 1440 + re-encode q80      (see §6)
  │
  ├─ 4. embed      ── POST {IMMICH_ML_URL}/predict
  │                   multipart: entries=<json>, image=<bytes>
  │                   → {"clip":"[0.01,-0.03,…]", "imageWidth":…, "imageHeight":…}
  │
  ├─ 5. search     ── one SELECT against smart_search JOIN asset       (see §5)
  │
  └─ 6. respond    ── matches ascending by distance,
                      duplicate = (best <= max_distance)               (see §8)
```

Steps 4 and 5 are each one network round trip. Nothing is written anywhere; a
failed request leaves no trace.

## 4. The ML contract

Immich's server talks to the ML container in
[`machine-learning.repository.ts`][mlrepo]. For CLIP image encoding,
[`encodeImage`][encodeimage] does exactly this:

```ts
const request = { [ModelTask.SEARCH]: { [ModelType.VISUAL]: { modelName } } };
```

With the enum values from [`schemas.py`][mlschemas] (`SEARCH = "clip"`,
`VISUAL = "visual"`; mirrored in TypeScript at [`machine-learning.repository.ts:14-26`][mlenums])
that serialises to:

```http
POST /predict HTTP/1.1
Content-Type: multipart/form-data; boundary=…

--…
Content-Disposition: form-data; name="entries"

{"clip":{"visual":{"modelName":"ViT-B-32__openai"}}}
--…
Content-Disposition: form-data; name="image"; filename="image"
Content-Type: application/octet-stream

<raw image bytes>
--…--
```

Built by [`getFormData`][getformdata], consumed by [`predict`][mlpredict].
Response:

```json
{ "clip": "[0.0123,-0.0456, … ]", "imageWidth": 1440, "imageHeight": 960 }
```

**The `clip` value is a string, not an array.** [`serialize_np_array`][serialize]
dumps the float32 array to JSON and returns the raw text, with the comment that
this "allows the client to use the array as a string without deserializing only to
serialize back to a string". [`BaseCLIPVisualEncoder._predict`][clipvisual] is what
calls it.

That string is *already a valid pgvector literal* — `'[0.01,-0.03]'::vector`
parses fine. So the Go code never parses the floats at all:

```go
var out struct{ Clip string `json:"clip"` }
// … then: rows, err := db.Query(q, out.Clip, ownerIDs, assetType, limit)
```

Skipping the parse is not just tidy: it removes float-formatting round-trip error
as a class of bug.

Other endpoints on the same container: [`GET /ping`][mlping] → `pong` (used by
`/healthz`), `GET /` → `{"message":"Immich ML"}`. Default port is
[3003][mlport], bound to `[::]`.

### There is no authentication, and none is available

[`POST /predict`][mlpredict] carries no API key, no token and no auth middleware.
Its only FastAPI dependencies are `update_state` (idle-shutdown bookkeeping) and
`get_entries` (form parsing). There is no auth-related setting anywhere in
[`config.py`][mlconfig] either.

The container's entire security model is **network isolation**: the stock compose
file publishes no host port for it, so only containers on the Immich network can
reach it. That is why clip-probe has to be a container on that network, and why
its own token matters — clip-probe is the authenticated front door to an
endpoint that has none.

Corollary for operators: never add `ports:` to `immich-machine-learning` to reach
it from elsewhere. That publishes an unauthenticated inference endpoint that will
happily burn your CPU for anyone who finds it.

### It is stateless, so a second copy is possible

Nothing in the ML container is per-instance: it loads ONNX models into memory and
answers requests. Running a private copy inside the clip-probe stack is therefore
legitimate (Option C in
[`docker-compose.standalone.yml`](docker-compose.standalone.yml)), but it is
rarely worth it:

- it does **not** remove the Immich dependency — the embeddings are in Postgres,
  so the shared network is required either way;
- `CLIP_MODEL` must still match whatever Immich indexed with, so a private
  container buys no freedom of model choice;
- it costs ~1 GB of resident RAM plus a model download.

The case where it does earn its keep: Immich's ML container is saturated by
indexing or facial recognition and you do not want probes queued behind that
work, or it holds a GPU you would rather not share.

### Model residency

[`model_ttl` defaults to 300 seconds][modelttl] — the model is evicted after five
minutes of inactivity, and the next request pays a multi-second reload. For
occasional probing that is most requests. `MACHINE_LEARNING_MODEL_TTL=0` plus
`MACHINE_LEARNING_PRELOAD__CLIP__VISUAL` keeps the visual encoder resident, at the
cost of RAM held permanently. This is why `timings.embedMs` is in the response:
without it, a cold start is indistinguishable from a slow query.

## 5. The search query

[`smart_search`][sstable] is one table with two columns:

```ts
@Table({ name: 'smart_search' })
@Index({ name: 'clip_index', using: 'hnsw', expression: `embedding vector_cosine_ops`, … })
export class SmartSearchTable {
  assetId!: string;      // PK, FK → asset.id, ON DELETE CASCADE
  embedding!: string;    // vector(512), storage external
}
```

The query below mirrors [`DuplicateRepository.search`][dupsql], minus the
self-exclusion, since our query image is not an asset:

```sql
SET LOCAL vchordrq.probes = $probes;

WITH cte AS (
  SELECT a.id,
         a."originalFileName",
         a."localDateTime",
         a."fileCreatedAt",
         a."ownerId",
         a.type,
         ss.embedding <=> $1::vector AS distance
  FROM smart_search ss
  INNER JOIN asset a ON a.id = ss."assetId"
  WHERE a."ownerId"   = ANY($2::uuid[])
    AND a."deletedAt" IS NULL
    AND a.visibility IN ('timeline', 'archive')
    AND a.type        = $3
  ORDER BY distance
  LIMIT $4
)
SELECT * FROM cte WHERE cte.distance <= $5;
```

Four things in there are load-bearing:

- **The CTE is not cosmetic, but it is not about results either.** Both forms
  return identical rows: if at least `limit` rows sit under the threshold then
  the nearest `limit` are all under it anyway, and if fewer do, every qualifying
  row is already inside the nearest `limit`. The CTE exists so the inner query
  stays a plain `ORDER BY … LIMIT`, the form an ANN index serves, and because it
  is the shape Immich uses. Since the difference is invisible in the output, only
  an `EXPLAIN` at production scale will tell you it regressed — which is why that
  check lives in the compat skill rather than in a test. With `all=true` the
  outer filter is simply dropped.
- **`<=>` is cosine distance** and must match the index's `vector_cosine_ops`.
  Using `<->` (L2) or `<#>` (inner product) silently bypasses the index. Immich
  uses `<=>` in [`search.repository.ts`][searchrepo] too.
- **`visibility IN ('timeline','archive')`** excludes `hidden` and `locked`. Per
  [`AssetVisibility`][visibility], `hidden` is specifically the video part of Live
  Photos and Motion Photos — embedding-bearing rows you never want surfaced as a
  duplicate of a still.
- **`ownerId` scoping** limits the blast radius of the database credential. See
  [§7](#7-security-model).

`SET LOCAL vchordrq.probes` is what Immich issues before the same query. Immich
also applies `ALTER DATABASE … SET vchordrq.probes = 1`
([`database.repository.ts`][probes]), so your connection inherits a sane default
and the `SET LOCAL` only matters if you raise `VCHORD_PROBES` for recall. On a
pgvector-HNSW deployment the GUC is `hnsw.ef_search` instead — detect which
extension is installed at startup, or let the `SET` fail softly.

The stock compose pins `ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0`,
so VectorChord is the relevant path.

## 6. Preprocessing parity

This is the subtlety that decides whether the numbers mean anything.

Immich does **not** embed the original file.
[`SmartInfoService.handleEncodeClip`][encode] calls `getForClipEncoding(id)`,
which [selects `withFiles(eb, AssetFileType.Preview)`][previewsel] — the *preview*
derivative. With [Immich's defaults][previewcfg] that is JPEG, long edge 1440,
quality 80, P3 colorspace.

So every vector in `smart_search` is the embedding of a 1440px q80 JPEG. Feed the
ML container a 48-megapixel original and you get the embedding of a different
rendering of the same scene. CLIP's own preprocessing resizes to 224×224 anyway,
so the discrepancy is small — but it is *systematic*, and it lands right in the
range where a `0.01` threshold decides yes or no.

Hence two things:

1. **The API asks for the right shape.** [`openapi.yaml`](openapi.yaml) documents
   the expected input as JPEG, long edge 1440, quality 80, EXIF orientation
   already applied. A client that sends that gets the most accurate answer and
   costs the server nothing.
2. **The server normalizes anything else** when `PREVIEW_PARITY=true` (default) —
   a handful of lines with `image/jpeg` plus `golang.org/x/image/draw`.

The pass-through rule matters: an input that is **already JPEG with a long edge
≤ 1440 is not touched**. Re-encoding it would add a second generation of JPEG loss
for no benefit. `query.normalized` in the response reports which path was taken,
so a surprising distance can be diagnosed from the payload alone.

<!-- ponytail: parity uses draw.CatmullRom, not Immich's libvips lanczos3 + P3
     colorspace. Residual bias is well under the threshold in practice; if
     calibration ever shows drift, shell out to vips or embed both renderings
     and take the min. -->

Two deliberate omissions:

- **No HEIC/RAW decoding.** Go's stdlib decodes JPEG, PNG, GIF and (via
  `x/image`) WEBP/TIFF. HEIC and camera RAW need cgo and libheif/libraw, which
  triples the image size and the build complexity. A clear `415` beats a fat
  container.
- **No EXIF auto-rotation.** CLIP is not rotation-invariant, and Immich's preview
  generator *does* apply orientation — so an unrotated query scores badly. The
  OpenAPI spec pushes this onto the client (`magick -auto-orient`). This is the
  one parity gap most likely to bite; if it does, apply orientation in step 3.

## 7. Security model

The service holds two things a caller must never be able to borrow: a database
credential that can read every user's assets, and network access to an
**unauthenticated** ML container.

### The token is this service's own

`x-api-key` is compared against `API_TOKEN` with
`subtle.ConstantTimeCompare`. It is not an Immich API key, it is never forwarded
to Immich, and `immich-server` is not contacted. Generate it with
`openssl rand -hex 32` — 32 bytes, so brute force is not a threat model.

Consequences to be aware of, since the token's lifecycle is now independent of
Immich's:

- **Revocation is manual**: change `API_TOKEN` and restart the container.
  Revoking an Immich API key does nothing here.
- **The token is not a user.** It carries no Immich identity, so scope has to come
  from configuration — that is what `OWNER_IDS` is for.

### `OWNER_IDS` is the blast radius, not an auth boundary

The SQL filters `a."ownerId" = ANY($2::uuid[])`. That is a *configuration* limit:
it decides which slice of the library this deployment can ever expose. It is not
per-caller authorization, because with a single shared token there is only one
caller. Anyone holding the token sees everything those owners own.

If you later want per-user scoping, that is the point at which validating an
Immich API key against `GET /api/users/me` and deriving `ownerId` per request
earns its extra dependency. Until then it would be a moving part with one setting.

### Non-negotiables

- **Connect to Postgres as a read-only role.** See below — this is the single
  most important line in this document.
- **Reject oversized bodies before reading them** — `http.MaxBytesReader`, not a
  length check after the fact.
- **TLS in front.** The token travels in a header. The Traefik labels in the
  README cover it. Do not expose the container port directly to the internet.
- **Request timeouts.** The ML container serialises inference per worker; a slow
  client must not pin it.
- **Never log the token**, and do not echo it in error messages.
- `/healthz` is unauthenticated on purpose (Docker and Traefik need it) and
  therefore reports no asset data beyond a count.

### The database credential

This service needs credentials to Immich's Postgres. That is the most
uncomfortable fact about the design, so it deserves a straight answer rather than
a footnote.

**It is unavoidable.** The embeddings exist in exactly one place —
[`smart_search`][sstable] — and no Immich HTTP endpoint exposes them. Reading them
means a database connection. There is no variant of this project that avoids it;
that is precisely why it is a third-party client of internal services rather than
a plugin.

**It does not need Immich's own credentials.** The query in [§5](#5-the-search-query)
touches two tables out of the 64 in the schema. Give it exactly those:

```sql
CREATE ROLE clip_probe LOGIN PASSWORD '…';
GRANT CONNECT ON DATABASE immich TO clip_probe;
GRANT USAGE  ON SCHEMA public    TO clip_probe;
GRANT SELECT ON smart_search, asset TO clip_probe;
```

Reusing `${DB_USERNAME}` works for a first run and is what the compose files ship
with, but it is a first-run convenience, not the destination.

What that role can actually reach:

| | |
| --- | --- |
| **Can read** | `asset`: `originalFileName`, `originalPath`, `ownerId`, `type`, `checksum` (sha1), `fileCreatedAt`, `localDateTime`, `isFavorite`, `visibility`. `smart_search`: one 512-float vector per asset. |
| **Cannot read** | The other 62 tables — including `user.password`, `api_key.key`, `asset_exif` (GPS coordinates), `asset_face` / `person` (who is in your photos), `album`, `session`, `partner`. |
| **Cannot do at all** | `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, DDL — on any table, including the two it reads. |

And the point most easily missed: **photos are not in the database.** Image bytes
are files on disk under `${UPLOAD_LOCATION}`. Even unrestricted `SELECT` across
all 64 tables would leak metadata, never a picture.

So the honest worst case, with the scoped role: *a compromised clip-probe leaks
the filenames, paths and capture dates of the configured owners' assets, plus
embeddings.* That is a real disclosure and worth the read-only role to contain —
but it is not "the keys to the library", and nothing about it is destructive.

Two cheap extras if you want to go further: put the role's password in a Docker
secret rather than the compose environment, and keep `OWNER_IDS` to the single
account you actually probe against, so the blast radius matches the use case.

## 8. The response object

Shape is in [`openapi.yaml`](openapi.yaml). The reasoning:

```jsonc
{
  "query":   { … },   // what was actually embedded
  "duplicate": true,  // the answer, as one boolean a script can branch on
  "maxDistance": 0.01,// the threshold actually applied
  "matches": [ … ],   // ascending by distance, possibly empty
  "timings": { … }    // four ints
}
```

- **`query` exists to make surprises self-diagnosing.** `normalized` says whether
  the file was re-encoded, `model` and `dimensions` make a model mismatch visible
  in the payload rather than only in logs. Nearly every "why is this distance
  weird" question is answered by those three fields.
- **`duplicate` is the product.** A shell script should need exactly one `jq -r
  .duplicate`. Everything else is for humans deciding what to do next.
- **`maxDistance` is echoed** because it is a query parameter with a default. A
  response saved to a file is otherwise uninterpretable six months later.
- **`similarity` is redundant** with `distance` and is included anyway: humans
  read `0.9969` far better than `0.0031`, and one subtraction is cheaper than
  every client reimplementing it.
- **`timings` costs four integers** and is the fastest way to see a cold ML
  container (`embedMs` in the thousands on the first call after startup).

Rejected: a `confidence: "exact" | "strong" | "weak"` enum. Those bands are
library-specific and would bake one person's calibration into the wire format.
The distance bands live in the README as guidance instead, where being wrong is
free.

Errors are `{"error": "<stable_code>", "message": "<human text>"}`. The code is
the contract; the message is not. Codes are enumerated in `openapi.yaml`.

## 9. Failure modes

| Symptom | Cause | Response |
| --- | --- | --- |
| Every distance ≈ 1.0, nothing ever matches | `CLIP_MODEL` ≠ Immich's model | startup dimension check fails `/healthz` |
| `ERROR: expected 512 dimensions, not 768` | same, at query time | `model_mismatch`, log configured vs actual dims |
| First request of the hour takes 4 s, `embedMs` huge | ML evicted the model after `model_ttl` | expected; see [§4 Model residency](#4-the-ml-contract) |
| Query takes seconds, CPU pegged | ANN index not used — filter pushed inside the CTE, or wrong distance operator | keep the query shape in [§5](#5-the-search-query) |
| Known-uploaded photo not found | asset has no `smart_search` row | `indexedAssets` in `/healthz` makes it visible |
| Distances slightly off across the board | parity skipped, or EXIF rotation not applied | check `query.normalized`; see [§6](#6-preprocessing-parity) |
| `502 ml_unavailable` on first call after deploy | ML container downloading the model | retry once with backoff |
| Results from an unexpected library | `OWNER_IDS` misconfigured | it is config, not auth — see [§7](#7-security-model) |
| `relation "asset" does not exist` | older Immich — the table was `assets` | fail at startup with a version hint |

## 10. Version coupling

Both runtime dependencies are internal to Immich and carry no compatibility
promise. Things that have moved before and can move again:

- the `asset` table name (it was plural in older versions),
- the `smart_search` column names and vector dimension,
- the `entries` JSON shape and the `clip` response key,
- the vector extension (pgvecto.rs → VectorChord) and its tuning GUC.

Mitigation is deliberately cheap rather than clever: a **startup preflight** that
runs one probe embedding end to end, asserts the dimension matches
`vector_dims(embedding)` from `smart_search`, and refuses to serve if either leg
fails. Pin the Immich version you tested against in the README, and treat a failed
preflight as the upgrade signal. No abstraction layer, no adapter interface for
hypothetical future schemas — one assertion that fails loudly.

The other half of the mitigation is knowing *where to look* when it does fail.
That lives in [`.claude/skills/immich-compat/SKILL.md`](.claude/skills/immich-compat/SKILL.md):
a scoped `git diff` over the twelve Immich files this project couples to, and a
symptom → source-file → local-file table. Run it before an Immich upgrade, or
after one breaks something.

## 11. Project layout

```
.
├── main.go              # config from env, wiring, http.Server, graceful shutdown
├── handler.go           # /v1/similar, /healthz, auth, wire types
├── ml.go                # /predict client (§4)
├── search.go            # the SQL (§5)
├── image.go             # decode + preview parity (§6)
├── image_test.go        # parity pass-through and rendering
├── ml_test.go           # the entries contract, verbatim embedding passthrough
├── search_test.go       # owner scoping and index usage (needs TEST_DB_URL)
├── openapi_test.go      # the spec and the wire types cannot drift apart
├── openapi.yaml
├── .claude/skills/immich-compat/SKILL.md   # where to look when an upgrade breaks it
├── Dockerfile
├── docker-compose.example.yml      # Immich's compose + clip-probe  (option A)
├── docker-compose.standalone.yml   # own stack, joins immich_default (option B)
└── .github/workflows/release.yml   # test, then build + push to ghcr.io
```

Flat package, no `internal/`, no `cmd/`. Five Go files is not a codebase that
needs a directory tree. (`auth.go` folded into `handler.go` the moment auth
became one constant-time compare.)

**Dependencies:** `github.com/jackc/pgx/v5` and `golang.org/x/image` at runtime,
plus `github.com/getkin/kin-openapi` for the contract test only — it is not
reachable from any non-test file, so it is absent from the binary. That is the
whole list. Go 1.27's `net/http` routing (`mux.HandleFunc("POST /v1/similar", …)`),
`log/slog`, `mime/multipart`, `crypto/subtle` and `encoding/json` cover everything
else — no router, no logging framework, no config library, no ORM.

**Dockerfile:** `golang:1.27-alpine` build stage → `gcr.io/distroless/static`. A
pure-Go binary with no cgo, so the runtime image is the binary plus CA certs.

## 12. Tests worth writing

One runnable check per piece of non-trivial logic, nothing more.

1. **Owner scoping** — insert two owners' embeddings into a throwaway Postgres,
   query with `OWNER_IDS` set to one, assert no row belongs to the other. This is
   the configured blast radius; it gets a test.
2. **Threshold semantics** — a filtered search returns a prefix of the unfiltered
   nearest results: same rows, same order, truncated.

   An `EXPLAIN`-based test asserting the ANN index appears in the plan was tried
   and removed. It cannot work on a synthetic fixture: Postgres picks a
   sequential scan below roughly production scale no matter how the query is
   written, and forcing the planner with `enable_seqscan = off` only made it
   choose the primary key instead. Nor is there a behavioural test to fall back
   on, because moving the threshold into the CTE returns identical rows (§5).
   Index usage was verified once against a live Immich instance and is re-checked
   on upgrade via the compat skill — the honest place for a property CI cannot
   observe.
3. **ML round trip** — an `httptest` server returning a canned `{"clip":"[…]"}`;
   assert the multipart `entries` field is exactly
   `{"clip":{"visual":{"modelName":"…"}}}` and that the string reaches the query
   verbatim.
4. **Parity pass-through** — a 1200px JPEG comes out byte-identical; a 4000px PNG
   comes out as a ≤1440 JPEG. Both report the right `normalized` flag.
5. **Spec conformance** — `openapi.yaml` is a published contract, so the wire
   types are compared field-for-field against it, in both directions: a field Go
   emits that the spec omits fails, and so does the reverse. Schema validation
   alone would miss the first and more common case. The error-code enum and the
   documented paths are checked the same way.

   Code generation from the spec (`oapi-codegen`) was the obvious alternative and
   was measured rather than assumed: it produces ~800 lines to replace ~100, needs
   the three `image/*` request content types collapsed into one to compile at all,
   and still leaves every parameter as a pointer to be defaulted by hand. The
   contract test buys the same protection against drift for about 150 lines and a
   test-only dependency, without making the published spec worse to satisfy a
   generator.

`go test ./...` against a `docker run` Postgres with the vector extension. No
fixtures framework, no mocks package.

## 13. Source references

| Claim | Source |
| --- | --- |
| `/predict` endpoint, `entries` + `image` form fields | [`main.py:166-181`][mlpredict] |
| `GET /ping` health endpoint | [`main.py:161-163`][mlping] |
| `ModelTask.SEARCH = "clip"`, `ModelType.VISUAL = "visual"` | [`schemas.py:28-38`][mlschemas] |
| Same enums, server side | [`machine-learning.repository.ts:14-26`][mlenums] |
| `encodeImage` builds `{clip:{visual:{modelName}}}` | [`machine-learning.repository.ts:205-209`][encodeimage] |
| Multipart body construction | [`machine-learning.repository.ts:228`][getformdata] |
| Embedding returned as a JSON-array **string** | [`transforms.py:72-76`][serialize] |
| CLIP visual encoder calls it | [`clip/visual.py:29-32`][clipvisual] |
| ML default port 3003, bound to `[::]` | [`config.py:87`][mlport] |
| **No auth on `/predict`** — only `update_state` and `get_entries` dependencies | [`main.py:166-169`][mlpredict] |
| No auth setting exists in ML config either | [`config.py`][mlconfig] |
| `model_ttl` defaults to 300 s (model eviction) | [`config.py:58`][modelttl] |
| `smart_search` table, `vector(512)`, cosine HNSW index | [`smart-search.table.ts`][sstable] |
| Duplicate search: CTE + `<=>` + outer distance filter | [`duplicate.repository.sql:146-177`][dupsql] |
| Smart search uses the same operator | [`search.repository.ts:326-327`][searchrepo] |
| `vchordrq.probes = 1` | [`database.repository.ts:118-119`][probes] |
| Immich embeds the **preview**, not the original | [`smart-info.service.ts:88-104`][encode] → [`asset-job.repository.ts:223-230`][previewsel] |
| Preview defaults: JPEG, 1440, q80, P3 | [`config.dto.ts:702-707`][previewcfg] |
| CLIP model default + `maxDistance: 0.01` | [`config.dto.ts:627-634`][clipcfg] |
| `AssetVisibility`: `hidden` = Live/Motion Photo video part | [`enum.ts:1178-1187`][visibility] |
| `user` table name, for resolving `OWNER_IDS` | [`user.table.ts:20`][usertbl] |
| No search-by-image endpoint exists | [`immich-openapi-specs.json`][openapi] — only `/search/smart` (text), `/search/metadata`, `/duplicates` |

[tree]: https://github.com/immich-app/immich/tree/202015ed95dc2aed6c03fc571067d18a1b46bf98
[mlpredict]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/main.py#L166-L181
[mlping]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/main.py#L161-L163
[mlschemas]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/schemas.py#L28-L38
[mlenums]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/repositories/machine-learning.repository.ts#L14-L26
[mlrepo]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/repositories/machine-learning.repository.ts
[encodeimage]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/repositories/machine-learning.repository.ts#L205-L209
[getformdata]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/repositories/machine-learning.repository.ts#L228
[serialize]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/models/transforms.py#L72-L76
[clipvisual]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/models/clip/visual.py#L29-L32
[mlport]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/config.py#L87
[mlconfig]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/config.py
[modelttl]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/config.py#L58
[sstable]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/schema/tables/smart-search.table.ts
[dupsql]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/queries/duplicate.repository.sql#L146-L177
[searchrepo]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/repositories/search.repository.ts#L326-L327
[probes]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/repositories/database.repository.ts#L118-L119
[encode]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/services/smart-info.service.ts#L88-L104
[previewsel]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/repositories/asset-job.repository.ts#L223-L230
[previewcfg]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/dtos/config.dto.ts#L702-L707
[clipcfg]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/dtos/config.dto.ts#L627-L634
[visibility]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/enum.ts#L1178-L1187
[usertbl]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/schema/tables/user.table.ts#L20
[openapi]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/open-api/immich-openapi-specs.json

## 14. Non-goals

- Uploading, writing or deleting anything in Immich.
- Checksum / perceptual-hash matching — use `POST /api/assets/bulk-upload-check`.
- A web UI. It is one HTTP endpoint; `curl` and `jq` are the UI.
- Batch endpoints. The ML container serialises inference anyway, so a batch
  endpoint would just move the loop from the client to the server. Loop in bash.
- Caching embeddings of query files. If you re-check the same file repeatedly,
  cache the *answer* on your side.
- Per-user authorization. See [§7](#7-security-model) for when it would be worth it.
- More than one Immich instance per container. Run two containers.
