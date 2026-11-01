# immich-clip-probe

<img src="brand/logo.svg" width="128" alt="immich-clip-probe logo">

[![build](https://github.com/dhcgn/immich-clip-probe/actions/workflows/release.yml/badge.svg?branch=main)](https://github.com/dhcgn/immich-clip-probe/actions/workflows/release.yml)
[![tools](https://github.com/dhcgn/immich-clip-probe/actions/workflows/tools.yml/badge.svg?branch=main)](https://github.com/dhcgn/immich-clip-probe/actions/workflows/tools.yml)
[![server release](https://img.shields.io/github/v/release/dhcgn/immich-clip-probe?filter=v*&label=server)](https://github.com/dhcgn/immich-clip-probe/releases/latest)
[![clip-check release](https://img.shields.io/github/v/release/dhcgn/immich-clip-probe?filter=tools/*&label=clip-check)](https://github.com/dhcgn/immich-clip-probe/releases?q=tools%2Fv&expanded=true)
[![Docker image](https://img.shields.io/badge/image-ghcr.io%2Fdhcgn%2Fimmich--clip--probe-2496ED?logo=docker&logoColor=white)](https://github.com/dhcgn/immich-clip-probe/pkgs/container/immich-clip-probe)
[![Go version](https://img.shields.io/github/go-mod/go-version/dhcgn/immich-clip-probe)](go.mod)
[![license](https://img.shields.io/github/license/dhcgn/immich-clip-probe)](LICENSE)


<img src="docs/screenshot_clip_check_desktop.png" width="400" alt="Clip Check desktop: green status lights for clip-probe, Immich and ImageMagick above a table of 7 checked photos, 2 match and 5 no match, each with the local and the nearest Immich thumbnail">

*Screenshot of Clip Check desktop interface*

> [!WARNING]
> **This repository is under active development. Do not expect anything to work.**
>
> Interfaces, configuration and the API shape can change without notice, and
> there is no stable release. It also depends on two *internal* Immich
> interfaces — the ML container's HTTP API and the Postgres schema — neither of
> which carries any compatibility promise, so an Immich upgrade can break it at
> any time. Read [ARCHITECTURE.md § Version coupling](ARCHITECTURE.md#10-version-coupling)
> before relying on it for anything.
>
> It is read-only against Immich and never uploads, writes or deletes — so the
> worst case is a wrong answer, not a damaged library. Treat every result as a
> hint to verify, not as proof.

> Ask your Immich library: **"do I already have a photo that looks like this one?"** — over HTTP, before you upload.

A small Go service in a container that exposes Immich's existing CLIP similarity
search for **files that are not in Immich yet**. Point it at a local JPEG, get back
the visually-nearest assets already in your library, with a distance score.

```console
$ curl -sS -H "x-api-key: $CLIP_PROBE_TOKEN" -F file=@query.jpg \
       https://clip-probe.home.example/v1/similar | jq
```

```json
{
  "query": {
    "fileName": "query.jpg",
    "bytes": 284193,
    "width": 1440,
    "height": 1080,
    "normalized": false,
    "model": "ViT-B-32__openai",
    "dimensions": 512
  },
  "duplicate": true,
  "maxDistance": 0.01,
  "matches": [
    {
      "assetId": "8f2c1d94-3a77-4c0e-9d51-6b0f2a1e77bc",
      "originalFileName": "IMG_4821.HEIC",
      "type": "IMAGE",
      "localDateTime": "2024-07-18T14:02:11.000Z",
      "fileCreatedAt": "2024-07-18T12:02:11.000Z",
      "ownerId": "3d1b0c6e-5f2a-4a1d-8e77-91c4f0b2a5de",
      "distance": 0.0031,
      "similarity": 0.9969,
      "links": {
        "web": "https://immich.home.example/photos/8f2c1d94-…",
        "thumbnail": "https://immich.home.example/api/assets/8f2c1d94-…/thumbnail"
      }
    }
  ],
  "timings": { "normalizeMs": 0, "embedMs": 143, "queryMs": 8, "totalMs": 154 }
}
```

Full contract: [`openapi.yaml`](openapi.yaml). Design and rationale:
[ARCHITECTURE.md](ARCHITECTURE.md).

## Why this exists

Immich already does all of this internally — it just never exposes it for an
*external* file:

| What you want | What Immich offers | Gap |
| --- | --- | --- |
| Search by **text** | `POST /api/search/smart` | text only, no image input |
| List duplicate **groups** | `GET /api/duplicates` | only assets *already uploaded and indexed* |
| Check by **checksum** | `POST /api/assets/bulk-upload-check` | byte-identical only — a re-export, a re-encode or a resize slips through |
| Search by **image** | — | **does not exist** |

So the only way to answer "is this picture already in there" today is to upload it
and look at the duplicates view afterwards. This service closes that loop: it
borrows the CLIP encoder from `immich-machine-learning` and runs the same
nearest-neighbour query [Immich's own duplicate detector runs][dupsql], against a
file on your disk.

**Non-goal: checksum matching.** If you want byte-identical detection, Immich's
`POST /api/assets/bulk-upload-check` already does it and is cheaper. This service
is deliberately about *visual* similarity.

## How it works (30 seconds)

```
  your file ──▶ clip-probe ──▶ immich-machine-learning  POST /predict  ──▶ 512-dim vector
                    │
                    └────────▶ immich_postgres   smart_search.embedding <=> $vector
                                                 └─▶ nearest assets + distance
```

No writes. No uploads. No Immich job queue involvement. Read-only on the database,
and it never talks to `immich-server` at all.

### Why two connections?

Because the two services hold different halves of the answer:

| | Role | Holds |
| --- | --- | --- |
| `immich-machine-learning` | **encode** — turn an image into a vector | nothing; it is a pure function with no memory of your library |
| `immich_postgres` | **compare** — find the nearest stored vector | every asset's embedding, in `smart_search` |

Your query file has no vector yet, so the ML container computes one. That vector
is meaningless alone — it needs the library's vectors to be compared against, and
those live in the database. If the ML container were stateful you could ask it
alone and skip Postgres; it isn't, so you can't.

Neither leg has an escape hatch. You cannot skip the database by using Immich's
HTTP API, because there is no search-by-image endpoint — that is the gap this
project exists to fill. And you cannot skip the ML container by computing CLIP
inside this service, because the result would have to match Immich's exact model
weights *and* preprocessing to be comparable, and the container already does that
correctly one hostname away.

## Quick start

### 1. Generate a token

```bash
openssl rand -hex 32
```

This is **this service's own token**, unrelated to Immich. Immich API keys are not
accepted and nothing is ever forwarded to Immich.

### 2. Find your Immich user id

The service is configured with the owner(s) whose library it searches. You have
pgAdmin in the stock compose, so the easiest route is one query against the
[`user`][usertbl] table:

```sql
SELECT id, email FROM "user";
```

Or one-off from the API, using an Immich API key you then throw away:

```bash
curl -sS -H "x-api-key: $IMMICH_API_KEY" https://immich.home.example/api/users/me | jq -r .id
```

### 3. Run it

Two ready-made compose files — pick one:

| | File | What it is |
| --- | --- | --- |
| **A** | [`docker-compose.example.yml`](docker-compose.example.yml) | The official Immich compose file, unchanged, with `clip-probe` appended. **Recommended** — no networking to configure. |
| **B** | [`docker-compose.standalone.yml`](docker-compose.standalone.yml) | Its own stack that joins Immich's network from outside. Leaves Immich's compose file untouched. |

Both read two new values from your `.env`:

```dotenv
CLIP_PROBE_TOKEN=<openssl rand -hex 32>
CLIP_PROBE_OWNER_IDS=3d1b0c6e-5f2a-4a1d-8e77-91c4f0b2a5de
```

```bash
docker compose up -d clip-probe && curl -fsS localhost:8080/healthz
```

Whichever you pick, the container has to reach `immich_postgres`: that is where
the embeddings live, and it publishes no host port. **No deployment avoids that
dependency** — including the bring-your-own-ML variant documented as Option C in
the standalone file.

`/healthz` verifies the database, the ML container, and that the embedding
dimension in `smart_search` matches the model you configured — see
[Model must match](#model-must-match).

## API

Every request carries `x-api-key: <API_TOKEN>`. See [`openapi.yaml`](openapi.yaml)
for the complete schema; the short version:

### `POST /v1/similar`

`multipart/form-data` with a single `file` part, or raw image bytes as the body
with a matching `Content-Type`.

| Query param | Default | Meaning |
| --- | --- | --- |
| `limit` | `10` | how many nearest assets to return |
| `max_distance` | `0.01` | cosine distance cut-off for `duplicate: true` |
| `all` | `false` | return the `limit` nearest regardless of `max_distance` |
| `type` | `IMAGE` | `IMAGE`, `VIDEO` or `all` |

Response fields, and why each one is there:

| Field | Why |
| --- | --- |
| `query.normalized` | tells you whether the service re-encoded your file, which is the first thing to check when a distance looks wrong |
| `query.model` / `query.dimensions` | makes a model mismatch obvious in the payload itself |
| `duplicate` | the yes/no answer, so a script can branch on one field |
| `maxDistance` | echoes the threshold actually applied, so a logged response is self-explaining |
| `matches[].distance` | cosine distance, `0` = identical direction |
| `matches[].similarity` | `1 - distance`; no extra information, it just reads better |
| `matches[].links.web` | paste-able URL to go look at the match |
| `timings` | four integers; the fastest way to see the ML container cold-starting |

Errors return `{"error": "<stable_code>", "message": "<human text>"}` — match on
`error`, never on `message`. Codes: `unauthorized`, `bad_request`,
`payload_too_large`, `unsupported_media`, `zero_size_image`, `ml_unavailable`,
`model_mismatch`, `db_unavailable`.

### `GET /healthz`

Unauthenticated. `200` with `{"status":"ok","db":"ok","ml":"ok","model":"…","dimensions":512,"indexedAssets":48213}`.

### Preparing the image: best practice

One command does everything that matters:

```bash
magick input.jpg -auto-orient -resize 1440x1440\> -quality 80 query.jpg
```

That is: bake in EXIF rotation, scale the **long** edge down to 1440 preserving
aspect ratio, never upscale (the `\>` does that), re-encode JPEG at quality 80.

#### What CLIP actually looks at

Understanding this makes the rules obvious. Immich's transform
([`OpenClipVisualEncoder.transform`][cliptransform]) does two things to whatever
it is given:

1. [`resize_pil`][transforms] scales the **shortest** edge to 224, keeping aspect
   ratio.
2. [`crop_pil`][transforms] takes a **centre crop of 224×224**.

So the model only ever sees the centre square of your photo. On a 3:2 landscape
shot, the left and right sixths are discarded before the model sees anything. On
a panorama, almost everything is.

That is also true of the assets already in your library, which is the point: your
query is only comparable if its centre square frames the same thing theirs does.

#### Rules

**Do**

- **Apply EXIF orientation** into the pixels. CLIP is not rotation-invariant, and
  Immich's preview generator rotates before embedding — an unrotated portrait
  photo scores badly against its own upright twin. The service does this for
  JPEG input; for anything else it is up to you.
- **Preserve aspect ratio.** It decides what survives the centre crop.
- **Resize the long edge to 1440.** Immich's own preview puts 1440 on the short
  edge, but with any resampler other than its libvips one the long-edge rendering
  measured closer to the stored vectors (ARCHITECTURE.md §6).
- **Flatten transparency** onto a solid colour first. Alpha is discarded, and a
  transparent background can land as black.
- **Leave the colour profile alone.** Converting between sRGB and Display P3
  shifts pixel values, and nothing downstream reads the profile to compensate.

**Don't**

- **Don't crop, pad or letterbox.** Padding pushes real content out of the centre
  crop and fills it with bars. A letterboxed 16:9 frame can embed closer to other
  letterboxed images than to the same scene uncropped.
- **Don't upscale.** Immich never does, so you would be comparing an interpolated
  image against a real one.
- **Don't square it yourself.** Immich's own centre crop is the reference; doing
  your own first crops twice.
- **Don't re-encode an already-conforming JPEG.** A second generation of JPEG
  loss moves distances for no gain.

#### What the service does for you

With `PREVIEW_PARITY=true` (the default) anything non-conforming is resized and
re-encoded server-side, a JPEG is rotated by its EXIF orientation, and an upright
JPEG already within 1440px is passed through untouched. `query.normalized` in the
response tells you which happened.

It does **not** decode HEIC or RAW, flatten transparency, or rotate formats other
than JPEG. For a JPEG straight off a camera, the command above is a bandwidth
saving rather than a correctness step.

#### Why 1440 and quality 80

Immich does not embed your original file — it embeds the **preview derivative**
([`handleEncodeClip`][encode] selects [`AssetFileType.Preview`][previewsel];
[defaults][previewcfg]). Every vector in `smart_search` is an embedding of a
1440px, quality-80 JPEG. Sending the same shape is both the most accurate option
and the cheapest, since the server then has nothing to do.

#### Where this breaks down

Panoramas, very wide crops and images with the subject far off-centre are all
weak cases, because the centre crop throws away the part that distinguishes them.
Two unrelated panoramas of the same coastline can sit closer together than a
panorama and its own tighter crop. Calibrate with `?all=true` before trusting
`duplicate` on that kind of material.

## Talking to Immich's ML container

**No authentication is involved, and none is available.** `immich-machine-learning`
exposes [`POST /predict`][mlpredict] with no API key, no token and no middleware —
the only FastAPI dependencies on the route are `update_state` (idle-shutdown
bookkeeping) and `get_entries` (form parsing). Its entire security model is that
the stock compose file publishes no host port for it, so only containers on the
Immich network can reach it.

That has three consequences worth knowing:

- **clip-probe just calls it.** One `POST` with a multipart body. Nothing to
  configure, nothing to rotate, no credential to leak.
- **It is stateless.** You may run a second copy of the image inside your own
  stack rather than sharing Immich's — see Option C in
  [`docker-compose.standalone.yml`](docker-compose.standalone.yml). It is usually
  not worth the ~1 GB of RAM, because it does not remove the database dependency
  and `CLIP_MODEL` still has to match whatever Immich indexed with.
- **Do not publish its port.** If you ever add `ports:` to
  `immich-machine-learning` to reach it from elsewhere, you have exposed an
  unauthenticated inference endpoint. Keep it on the internal network and let
  clip-probe be the thing with a token in front of it.

### Cold starts

The ML container unloads a model after
[`MACHINE_LEARNING_MODEL_TTL`][modelttl] seconds of inactivity — **300 by
default**. If you probe occasionally rather than continuously, most requests will
pay a multi-second model load, visible as a large `timings.embedMs` in the
response.

To keep the visual encoder resident:

```yaml
environment:
  MACHINE_LEARNING_MODEL_TTL: 0
  MACHINE_LEARNING_PRELOAD__CLIP__VISUAL: ViT-B-32__openai
```

Costs about 1 GB of RAM, and on Immich's own container it affects Immich too.
Both compose files carry this as a commented block.

## Database access

**Yes, this service needs credentials to Immich's Postgres.** It is the one
uncomfortable requirement, so here is the straight version.

It is unavoidable: the embeddings exist only in `smart_search`, and no Immich HTTP
endpoint exposes them. But it does **not** need Immich's own `${DB_USERNAME}`. The
query touches 2 of the 64 tables in the schema, so grant exactly those:

```sql
CREATE ROLE clip_probe LOGIN PASSWORD 'pick-something-long';
GRANT CONNECT ON DATABASE immich TO clip_probe;
GRANT USAGE  ON SCHEMA public    TO clip_probe;
GRANT SELECT ON smart_search, asset TO clip_probe;
```

Then point `DB_URL` at that role. The compose files ship with `${DB_USERNAME}` as
a first-run convenience; this is where you should end up.

| | |
| --- | --- |
| **Can read** | filenames, original paths, owner, type, sha1 checksums, timestamps, favourite/visibility flags, and one 512-float vector per asset |
| **Cannot read** | the other 62 tables — `user.password`, `api_key.key`, `asset_exif` (GPS), faces and person names, albums, sessions, partner shares |
| **Cannot do at all** | insert, update, delete, truncate or DDL, on any table |

And the part most people miss: **your photos are not in the database.** Image bytes
are files on disk under `${UPLOAD_LOCATION}`. Even unrestricted `SELECT` on every
table would leak metadata, never a picture.

Worst case with the scoped role: a compromised clip-probe discloses the filenames
and capture dates of the owners you configured. Worth containing — but not the
keys to the library, and nothing about it is destructive. Full reasoning in
[ARCHITECTURE.md § Security model](ARCHITECTURE.md#7-security-model).

## Running against a remote Immich

Two ways, depending on whether you are deploying or iterating on the code.

**Deploying — run clip-probe on the Immich host.** Copy
[`docker-compose.standalone.yml`](docker-compose.standalone.yml) to that machine
and point `networks.immich.name` at the project's network. Find it with:

```bash
docker network ls | grep immich
```

A compose project named `immich-staging` gives a network `immich-staging_default`,
and the container names follow the same pattern
(`immich_postgres_staging`, `immich_machine_learning_staging`). No ports are
exposed to the host and nothing is tunnelled.

**Iterating locally — tunnel to the containers over SSH.** Neither Postgres nor
the ML container publishes a host port, so tunnel to their container IPs, which
the Docker host itself can reach:

```bash
ssh -N -L 15432:$(ssh host 'docker inspect -f "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}" immich_postgres'):5432 \
       -L 13003:$(ssh host 'docker inspect -f "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}" immich_machine_learning'):3003 host
```

Then run the binary against the tunnels:

```bash
DB_URL=postgresql://postgres:PASSWORD@localhost:15432/immich IMMICH_ML_URL=http://localhost:13003 API_TOKEN=dev OWNER_IDS=<uuid> go run .
```

Container IPs change when containers are recreated, so re-read them after a
`docker compose up`. Do not solve this by publishing the ML port — that exposes
an unauthenticated inference endpoint, as described above.

## Configuration

| Variable | Default | Notes |
| --- | --- | --- |
| `API_TOKEN` | — | **required**, this service's own token. `openssl rand -hex 32` |
| `OWNER_IDS` | — | **required**, comma-separated Immich user UUIDs to search |
| `DB_URL` | — | **required**, `postgresql://user:pass@database:5432/immich`. Use a [read-only role](#database-access), not Immich's own |
| `IMMICH_ML_URL` | `http://immich-machine-learning:3003` | [default ML port][mlport] |
| `PUBLIC_IMMICH_URL` | — | base for `links`; omit and `links` is left out. No requests are made to it |
| `LISTEN_ADDR` | `:8080` | |
| `CLIP_MODEL` | `ViT-B-32__openai` | [Immich's default][clipcfg]; **must equal** yours |
| `MAX_DISTANCE` | `0.01` | [same default Immich uses][clipcfg] for duplicate detection |
| `DEFAULT_LIMIT` | `10` | |
| `MAX_UPLOAD_BYTES` | `52428800` | 50 MiB |
| `PREVIEW_PARITY` | `true` | normalize non-conforming input to 1440px / q80 JPEG |
| `VCHORD_PROBES` | `0` (auto) | IVF lists to scan. Auto = `ceil(lists/8)` from `clip_index`, what [Immich's own search][probes] uses; the database default of 1 misses matches once the index has more than one list. Raise for better recall at higher cost |
| `LOG_LEVEL` | `info` | |

### Model must match

CLIP embeddings from different models are not comparable — different dimensions,
and even at equal dimensions a different vector space. If your Immich
**Administration → Settings → Machine Learning → Smart Search** model is not
`ViT-B-32__openai`, set `CLIP_MODEL` to the same value.

On startup the service compares a probe embedding against `vector_dims(embedding)`
from `smart_search`. A mismatch fails the health check loudly instead of silently
returning nonsense neighbours.

## Recipes

Scan a folder, print what already looks uploaded (or use
[`clip-check`](#tools-clip-check), which also handles HEIC and RAW):

```bash
for f in *.jpg; do d=$(curl -sS -H "x-api-key: $CLIP_PROBE_TOKEN" -F "file=@$f" https://clip-probe.home.example/v1/similar); [ "$(jq -r .duplicate <<<"$d")" = true ] && echo "DUP $f -> $(jq -r '.matches[0].originalFileName' <<<"$d")"; done
```

Calibrate your own threshold by looking at the nearest matches with no cut-off:

```bash
curl -sS -H "x-api-key: $CLIP_PROBE_TOKEN" -F file=@photo.jpg 'https://clip-probe.home.example/v1/similar?all=true&limit=5' | jq '.matches[] | {originalFileName, distance}'
```

Rough bands on a typical library — **measure your own**, these are not universal:

| distance | usually means |
| --- | --- |
| `< 0.005` | the same image, re-encoded or resized |
| `0.005 – 0.02` | same scene, adjacent frame or a light edit |
| `0.02 – 0.10` | same subject or location, different photo |
| `> 0.10` | merely similar-looking |

## Tools: clip-check

Two small clients for this service: the **`clip-check`** CLI and the **Clip
Check** desktop app (Windows, Linux). Give either a file, several files or a
folder, and every image gets its nearest Immich asset and the distance to it.

```console
$ clip-check ~/Pictures/2019
MATCH 0.0042  2019/IMG_1234.jpg → IMG_1234.HEIC 2019-06-25 https://immich.home.example/photos/8f2c…
NO    0.1873  2019/IMG_9999.jpg → IMG_0001.jpg 2017-03-02 https://immich.home.example/photos/1a7e…
SKIP          2019/IMG_5524.heic  (needs ImageMagick to decode .heic)
3 files: 1 match, 1 no match, 1 skipped, 0 errors
```

<img src="docs/screenshot_clip_check_cli.png" alt="clip-check in PowerShell: the help text, then one file checked as text (MATCH 0.0012) and as --json">

The desktop app does the same with **Add files**, **Add folder** or a drop onto
the window. It shows the results as a table: a thumbnail of the local file next
to the matching Immich asset's thumbnail, with an **Open in Immich** link per
row. It also has a settings panel. The Immich thumbnails are optional and need
the Immich URL and an API key with the `asset.view` permission.

<img src="docs/screenshot_clip_check_desktop.png" alt="Clip Check desktop: green status lights for clip-probe, Immich and ImageMagick above a table of 7 checked photos, 2 match and 5 no match, each with the local and the nearest Immich thumbnail">

**Install.** Binaries are on the [releases page](https://github.com/dhcgn/immich-clip-probe/releases)
under `tools/v*` tags: `clip-check-<os>-<arch>` for Windows, Linux and macOS,
and `clip-check-desktop-windows-amd64.exe` / `clip-check-desktop-linux-amd64`.
The Linux desktop binary needs GTK 4 and WebKitGTK 6.0 (Debian/Ubuntu:
`libgtk-4-1 libwebkitgtk-6.0-4`). Or build the CLI yourself:

```bash
go install github.com/dhcgn/immich-clip-probe/tools/clip-check@latest
```

**Configure.** Both tools read the same file, `clip-check/config.yaml` in the
user config folder (`%AppData%` on Windows, `~/.config` on Linux,
`~/Library/Application Support` on macOS); the desktop's settings panel writes it.

```yaml
server: https://clip-probe.home.example
token: <API_TOKEN>
max_distance: 0.01 # the nearest asset counts as a match at or below this
limit: 1           # nearest assets listed per file
workers: 4         # files checked in parallel
magick: ""         # path to magick.exe; empty finds it, "off" never uses it
immich_url: ""     # optional, desktop only: Immich thumbnails of the matches
immich_api_key: "" # with immich_url; asset.view permission is enough
```

<img src="docs/screenshot_clip_check_desktop_settings.png" alt="Clip Check desktop settings: clip-probe server URL, API token, match distance, matches per file and files in parallel; optional Immich URL and API key for thumbnails; optional ImageMagick path, found automatically">

Every key also works as an environment variable (`CLIP_CHECK_TOKEN`,
`CLIP_CHECK_MAX_DISTANCE`, …) and as a CLI flag (`--token`, `--max-distance`, …).
A flag beats the environment, which beats the file. `clip-check health` shows
the server's state and whether ImageMagick was found. `--json` prints one
result per line. The exit code is 1 if any file failed.

One `--json` result, formatted ([docs/clip_check_result.json](docs/clip_check_result.json)):

```json
{
    "path": "C:\\image.jpg",
    "status": "match",
    "distance": 0.0012026133661843241,
    "matches": [
        {
            "assetId": "ac132d54-a585-4d41-b85e-8efa421b1843",
            "originalFileName": "DSC_0532.JPG",
            "localDateTime": "2026-05-01T11:06:20.12Z",
            "type": "IMAGE",
            "distance": 0.0012026133661843241,
            "links": {
                "web": "https://example.org/photos/ac132d54-a585-4d41-b85e-8efa421b1843"
            }
        }
    ],
    "renderedBy": "imagemagick"
}
```

**Install ImageMagick too.** When it is found (on `PATH`, or in `Program Files`
on Windows), every image is rendered locally the way Immich renders the preview
it embeds, and only that rendering, a few hundred KB, is uploaded
([ARCHITECTURE.md §6](ARCHITECTURE.md#6-preprocessing-parity)). Without it,
JPEG, PNG, GIF, WebP and TIFF go up as they are and are normalized by the
service. HEIC, RAW (DNG, CR2, NEF, …), AVIF and JXL are skipped.

On a live library, with 70 originals checked against their own assets:
- ImageMagick came closer for 28 of 36 JPEGs.
- It was the only way to check the 15 HEIC, 11 RAW and 7 JXL files at all.

**Photos you know are in Immich show "no match" at 0.02–0.05?** They were
probably added before your Immich was upgraded to 3.3.0. Run Smart Search
"All" once, and check again only after it has finished
([Limitations](#limitations)).

The tools always ask for the nearest matches with no cut-off, and decide
"match" against `max_distance` locally. So a distance just past the threshold is
still shown, and changing the threshold needs no new upload.

Folders are walked recursively for Immich's image types, skipping dotfiles and
dot-folders. Videos are not checked, because Immich embeds a frame of a video,
not the file.

**Release** the tools with their own tag. The `tools` workflow builds every
binary and publishes the GitHub Release, independently of the server's `v*` tags:

```bash
git tag -a tools/v0.1.0 -m "tools/v0.1.0" && git push origin tools/v0.1.0
```

## Image tags and releases

| Tag | What it is |
| --- | --- |
| `:latest`, `:main` | The tip of `main`. Moves on every push, may be broken — see the warning at the top |
| `:0.1.0` | A tagged release, built from that exact commit |
| `:0.1` | The newest patch within that minor version |

Given the state of the project, `:latest` is the honest default and is what the
compose files use. Pin a version tag once you care about reproducibility.

Cutting a release is a tag push; CI builds the images and opens the GitHub
Release with generated notes:

```bash
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

## Limitations

- **Upgraded to Immich 3.3.0 or later? Run Smart Search "All" once.** Version
  3.3.0 changed how Immich computes embeddings internally: previews are now
  resampled in linear light, and the ML container got a new inference engine.
  Assets added *before* the upgrade keep vectors from the old pipeline. Their own
  photo, embedded today, sits about 0.02–0.05 away from that stored vector, so it
  never gets under `max_distance` 0.01.

  On one library, assets uploaded before the upgrade measured 0.017–0.042 from
  their own vector, and assets uploaded after it 0.002–0.009. The fix is in
  Immich: **Administration → Jobs → Smart Search → All** re-embeds every asset
  with the current pipeline. "Missing" is not enough, since those assets already
  have a vector.

  "All" starts by deleting every stored vector, then re-embeds asset by asset.
  Until it finishes, a photo that has not been re-embedded yet has no vector at
  all and cannot be found. Wait until `indexedAssets` in `/healthz` (the desktop
  app's status bar shows it) is back to its old count before you trust a "no
  match".
- **Only indexed assets are searchable.** An asset with no `smart_search` row
  (Smart Search job never ran, or was disabled) is invisible here. `indexedAssets`
  in `/healthz` makes that visible.
- **CLIP is semantic, not forensic.** Two different photos of the same sunset can
  land under `0.01`; a tight crop of one image can land above it. Treat
  `duplicate` as a strong hint, not proof.
- **No HEIC or RAW.** Those need cgo and libheif/libraw. Convert first; you get a
  clear `415` otherwise.
- **Read-only, single Immich instance.**
- **Version-coupled.** It reads Immich's internal database schema and internal ML
  API, neither of which is a stable public contract. See
  [ARCHITECTURE.md § Version coupling](ARCHITECTURE.md#10-version-coupling).

## Verified against

Everything above was read out of the Immich source tree, not inferred. All links
in these docs are permalinks to commit
[`202015e`](https://github.com/immich-app/immich/tree/202015ed95dc2aed6c03fc571067d18a1b46bf98)
on `main` (2026-09-19), which reports server version `3.2.0`. Note this commit is
*not* an ancestor of the `v3.2.x` release tags, so line numbers are pinned by SHA.

**When a future Immich release breaks this**, the repo ships a Claude Code skill
with the coupling map: [`.claude/skills/immich-compat/SKILL.md`](.claude/skills/immich-compat/SKILL.md).
It gives you one scoped `git diff` across the twelve Immich files that matter, and
a symptom → where-to-look table. Worth running *before* an upgrade too.

[dupsql]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/queries/duplicate.repository.sql#L146-L177
[encode]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/services/smart-info.service.ts#L88-L104
[previewsel]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/repositories/asset-job.repository.ts#L223-L230
[previewcfg]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/dtos/config.dto.ts#L702-L707
[clipcfg]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/dtos/config.dto.ts#L627-L634
[mlport]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/config.py#L87
[mlpredict]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/main.py#L166-L181
[cliptransform]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/models/clip/visual.py#L71-L76
[transforms]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/models/transforms.py#L14-L27
[modelttl]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/machine-learning/immich_ml/config.py#L58
[probes]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/repositories/database.repository.ts#L361-L371
[usertbl]: https://github.com/immich-app/immich/blob/202015ed95dc2aed6c03fc571067d18a1b46bf98/server/src/schema/tables/user.table.ts#L20

## License

AGPL-3.0, matching Immich. Not affiliated with the Immich project — this is a
third-party client of its internal services.
