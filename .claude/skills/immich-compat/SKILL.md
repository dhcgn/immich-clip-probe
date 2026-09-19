---
name: immich-compat
description: Diagnose and fix immich-clip-probe when an Immich upgrade breaks it, or check a new Immich release for breaking changes before upgrading. Use when /healthz fails, embeddings come back the wrong dimension, distances look wrong or all matches vanish, or on errors like "relation \"asset\" does not exist", model_mismatch, or ml_unavailable after an Immich version bump.
---

# Immich compatibility check

This project is a third-party client of two **internal** Immich interfaces: the ML
container's HTTP API and the Postgres schema. Neither is a stable contract. This
skill is the list of places where that coupling lives, so a break can be found by
reading rather than guessing.

## 0. Get both versions

The docs pin the commit everything was verified against. Find it:

```bash
grep -m1 -o '[0-9a-f]\{40\}' ARCHITECTURE.md
```

Get the Immich source to diff against — a local checkout if there is one
(`D:\immich` on the original author's machine), otherwise:

```bash
git clone --filter=blob:none https://github.com/immich-app/immich
```

Find what the running instance actually is: Immich web UI → Administration →
About, or `docker inspect immich_server --format '{{.Config.Image}}'`.

## 1. Run this diff first

It is the whole skill in one command. Everything that can break clip-probe lives
in these twelve files:

```bash
git diff <pinned-sha> <new-tag> -- \
  server/src/repositories/machine-learning.repository.ts \
  server/src/repositories/asset-job.repository.ts \
  server/src/repositories/database.repository.ts \
  server/src/services/smart-info.service.ts \
  server/src/schema/tables/smart-search.table.ts \
  server/src/schema/tables/asset.table.ts \
  server/src/queries/duplicate.repository.sql \
  server/src/dtos/config.dto.ts \
  machine-learning/immich_ml/main.py \
  machine-learning/immich_ml/schemas.py \
  machine-learning/immich_ml/models/transforms.py \
  machine-learning/immich_ml/config.py
```

Empty diff → the break is not a compatibility break. Look at deployment instead:
network name, container names, credentials, `MACHINE_LEARNING_MODEL_TTL`.

## 2. Coupling points

Work down this table. Each row: what breaks, where Immich defines it, what to fix
here. `ARCHITECTURE.md` §13 has permalinks to every one of these at the pinned
commit — use them to see what the code *used* to say.

| Symptom | Read in Immich | Fix in |
| --- | --- | --- |
| `ml_unavailable`, 422 from `/predict` | `machine-learning/immich_ml/main.py` — the `predict` route and `get_entries` | `ml.go` |
| Request rejected, `entries` shape wrong | `machine-learning.repository.ts` → `encodeImage`, `getFormData`; enum values in `schemas.py` (`ModelTask.SEARCH`, `ModelType.VISUAL`) | `ml.go` |
| Embedding no longer parses as a vector literal | `machine-learning/immich_ml/models/transforms.py` → `serialize_np_array`. If it stops returning a JSON-array **string**, the zero-parse trick in §4 dies | `ml.go`, `search.go` |
| `relation "..." does not exist` | `server/src/schema/tables/` — table names have been renamed before (`assets` → `asset`) | `search.go` |
| `column "..." does not exist` | `smart-search.table.ts`, `asset.table.ts` | `search.go` |
| Dimension mismatch / `model_mismatch` | `config.dto.ts` → the `clip.modelName` default changed, or the admin changed it. Vectors from different models are not comparable | `CLIP_MODEL`, then **re-run Smart Search in Immich** |
| Everything got slow, CPU pegged | `duplicate.repository.sql` → `DuplicateRepository.search`. Check the CTE shape and the distance operator still match | `search.go` |
| Vector extension or tuning GUC changed | `database.repository.ts` → search for `probes` / `ef_search`; also the postgres image tag in Immich's compose | `search.go`, `VCHORD_PROBES` |
| Distances subtly shifted for everything | `config.dto.ts` → `image.preview` defaults (size 1440, quality 80), and `smart-info.service.ts` → `handleEncodeClip` may select a different `AssetFileType` | `image.go`, `openapi.yaml`, README |
| Assets missing that should match | `enum.ts` → `AssetVisibility` values, used in the `visibility IN (...)` filter | `search.go` |
| Wrong ML port / endpoint | `machine-learning/immich_ml/config.py` → `immich_port` | `IMMICH_ML_URL` default |

## 3. Two changes that would need a design decision, not a patch

- **The ML container grows authentication.** Check whether `predict` in `main.py`
  gained a security dependency beyond `update_state` / `get_entries`, and whether
  `config.py` gained an auth setting. If so, clip-probe needs to hold that
  credential too — re-read ARCHITECTURE.md §7 before wiring it in.
- **Immich adds a real search-by-image endpoint.** Check
  `open-api/immich-openapi-specs.json` for new paths under `/search`. If one
  appears, most of this project should be deleted in favour of it. That is a good
  outcome; say so rather than maintaining a workaround.

## 4. After fixing

Re-pin the docs. The commit SHA appears in every permalink across three files:

```bash
grep -rl '<old-sha>' . | xargs sed -i 's/<old-sha>/<new-sha>/g'
```

Then update the version line in README "Verified against" and the header of
ARCHITECTURE.md, and re-check anything ARCHITECTURE.md §13 asserts a **line
number** for — line numbers drift even when code does not.

Verify:

```bash
go test ./... && docker compose up -d --build clip-probe && curl -fsS localhost:8080/healthz
```

`/healthz` catches the dimension mismatch. It does not catch a wrong `entries`
shape or a changed preview default, so also probe a file you know is in the
library and confirm the distance is still near zero — a silent drift from 0.003
to 0.04 is the failure this skill exists to catch.
