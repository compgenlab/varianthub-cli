# REST API

`cganno server` runs an asynchronous annotation service over HTTP: it exposes the same
annotation engine as the CLI, backed by the same snapshot config and cache. Annotation can be
expensive (external tools, large lookups), so requests are **queued** — a submit returns a
**job id** immediately; a worker pool annotates in the background; the caller **polls** by job
id and fetches JSON results when the job is done. Jobs and results persist in a dedicated
SQLite database.

Every request (fast or slow) goes through the queue, but a submit may include **`?wait=<seconds>`**
(capped by `submit_wait`, default 10s) to block briefly for the job to finish and return the
**results inline** — so quick lookups come back done in a single round trip, while slow jobs fall
back to polling. The same `?wait=` works on `GET …/jobs/{id}/results` for a poller that would
rather block than spin. The browser form uses this so simple variants render immediately.

## Configuration

Add a `[server]` block to `config.toml` (see [`config.example.toml`](../config.example.toml)):

```toml
[server]
endpoint   = "127.0.0.1:8080"                 # IP:port to listen on (bind localhost behind a proxy)
master_key = "change-me-to-a-secret"          # HMAC key signing /v1 API tokens (omit if require_token=false)
require_token = true                           # bearer-token auth on /v1 (false = open, tokenless public API)
workers    = 2                                 # async worker pool size — jobs run at once (default 1)
db         = "$CGANNO_HOME/cganno_server.db"   # job-queue + results DB (default ./cganno_server.db)

# Large-job performance (VCF uploads)
max_chunk_variants = 2000                      # split a job into ≤N-variant chunks annotated in parallel (0 = off)
annotate_threads   = 0                         # per-job chunk parallelism (0 = all cores)
submit_wait        = "10s"                     # a submit (or results ?wait=) blocks up to this for the job; fast jobs return inline ("0" = always async)

# Retention
job_ttl = "24h"                                # GC terminal jobs + results older than this ("" = 24h default; "0" = keep forever)

# Public-service abuse protection
max_jobs_per_ip = 2                            # per-IP concurrent running-job cap (fair queue; <=0 = unlimited)
rate_per_min    = 30                           # per-IP submit throttle, requests/min (<=0 = unlimited)
rate_burst      = 10                           # per-IP throttle burst
trusted_proxies = ["127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]
allow_tools_unauth = false                     # let unauthenticated /ui trigger type="tool" sources (VEP/ANNOVAR)

# Browser UI
ui_enabled       = true                        # serve the browser form + /ui/* twins
ui_require_token = false                        # also require the bearer token on /ui/*
```

`workers` bounds how many jobs run **at once**; `annotate_threads` bounds how many cores a
**single** job may use (its loci are split into `max_chunk_variants`-sized chunks annotated in
parallel, so one large VCF isn't stuck on a single core). `workers × annotate_threads` can
oversubscribe cores — for a server dominated by occasional big jobs, `workers = 1–2,
annotate_threads = 0` (all cores) is a good split.

Run it:

```sh
cganno server                 # uses the [server] endpoint
cganno server -addr :9000     # override the endpoint
```

The server uses the active snapshot (config `default_snapshot`, or the global `-snapshot`
flag) and the annotation cache from `[database]`. It runs the **full locus path**, including
`type="tool"` sources (VEP/ANNOVAR) — same as `cganno annotate <locus>`.

## Authentication

The `/v1/*` API is authenticated with an HMAC-signed bearer token. **On startup the server
prints one valid token to stdout** (logs go to stderr):

```sh
$ cganno server
eyJzdWIiOiJjZ2Fubm8i….fJfpi6WBBCUX-5G4FrXnYh78meDvTtjMJIy3sb0pO6E   # <- the token (stdout)
2026/07/05 01:28:38 cganno server: snapshot "2026-07", 2 worker(s); listening on http://127.0.0.1:8080
```

Send it as `Authorization: Bearer <token>` on every `/v1` request. A token is
`b64url(payload).b64url(HMAC-SHA256(master_key, b64url(payload)))`; any token correctly signed
by `master_key` is accepted (tokens do not expire in this version). The browser form and its
`/ui/*` endpoints are **open** (no token) for local/trusted-network convenience.

**Open public API** — set `require_token = false` to serve `/v1` **without** a bearer token (no
`Authorization` header, no startup token, `master_key` optional). The per-IP throttle, fair queue,
and tool-source gate still apply, so an open server is still protected against abuse. Use this for
a genuinely public service (behind a reverse proxy); keep the default (`true`) anywhere the API
should be authenticated.

## Endpoints

### Authenticated API (`/v1`, bearer token)

| method + path | body | response |
| --- | --- | --- |
| `GET /v1/annotations` | — | the snapshot's sources + annotation fields, with the default set marked (for discovery/selection) |
| `POST /v1/annotate` | `{ "locus": "chrom:pos:ref:alt", "annotations": "all"｜["a",…] }`; optional `?wait=<sec>` | `202 { "job_id" }`, or `200 { "job_id", "status":"done", "n_variants", "results":[…] }` if it finishes within `wait` |
| `POST /v1/annotate/vcf` | `multipart/form-data`: file field `vcf`, optional `annotations` form field; optional `?wait=<sec>` | `202 { "job_id" }` (or inline results, as above) |
| `GET /v1/jobs` | `?status=&limit=&offset=` (all optional) | `{ "jobs": [ … ], "limit": N, "offset": M, "scoped": bool }`, newest first |
| `GET /v1/jobs/{id}` | — | job status object |
| `GET /v1/jobs/{id}/results` | — | the results array (see below), or `409` if not finished |

Open (no token): `GET /healthz` → `{"status":"ok","snapshot":"…","assembly":"…"}` (for proxy health
checks) and `GET /version` → `{"version":"…"}`.

**Browsing your requests.** `GET /v1/jobs` (and `/ui/jobs`) lists jobs newest-first, but is
**scoped to the requester** so one user can't browse another's history on an open server:
unauthenticated requests see only their own jobs — by **session** (the `cganno_session` cookie the
browser gets on first load, or an `X-Cganno-Session: <id>` header an API client sends), falling
back to the client IP when there is no session. An **authenticated** request (valid bearer token)
is treated as an admin and sees **all** jobs; the `scoped` field in the response says which applies.
Each job carries a `label` (the locus, or the uploaded VCF's filename) for display.

Submitting more than the configured per-IP rate returns `429`. On a public server, unauthenticated
requests that select a `type="tool"` annotation are rejected with `403` unless `allow_tools_unauth`
is set (authenticated `/v1` requests are always allowed).

`annotations` is optional — omit it for the snapshot's **default** annotations, pass `"all"`
for every annotation, or a **list of names** (from `GET /v1/annotations`) to select specific
ones. Unknown names / malformed loci are rejected at submit time with `400`.

`GET /v1/jobs/{id}` returns:

```json
{ "job_id": "…", "kind": "locus", "snapshot": "2026-07", "selection": "",
  "status": "queued|running|done|error", "error": "", "n_variants": 1,
  "created_at": 1783229365, "started_at": 1783229365, "finished_at": 1783229365 }
```

### Results

Results honor the same JSON shape as the CLI's [`--format json`](io-formats.md#annotate-output):
an array of per-variant objects, in input order, each carrying its coordinates and a
name→value map (every selected annotation is a key, `null` when the locus has no match):

```json
[ { "chrom": "chr2", "pos": 200, "ref": "C", "alt": "T",
    "annotations": { "auto_id": "chr2_200_C_T", "tstv": "TS" } } ]
```

For a **VCF upload** there is one object **per line, in file order** — a multi-allelic ALT is
split into one object per allele, each carrying its own `chrom/pos/ref/alt`.

### Browser form (open, no token)

- `GET /` — a minimal HTML form with three input modes: a single `chrom:pos:ref:alt` **locus**, a
  **batch** of loci (one per line), or a **VCF file** upload. Tick the annotations to return
  (fetched from `/ui/annotations`, defaults pre-checked; select-all/none buttons), submit. Its
  JavaScript posts the job, polls its status, renders the result as a table, and offers
  **JSON / CSV / TSV** downloads. A **Recent requests** panel lists this browser session's prior
  submissions (scoped by the `cganno_session` cookie) — click a completed one to re-view its
  results. Batch and VCF modes post to `/ui/submit/vcf` (batch synthesizes
  a sites-only VCF client-side). Disabled entirely when `ui_enabled = false`.
- `GET /ui/annotations`, `POST /ui/submit`, `POST /ui/submit/vcf`, `GET /ui/jobs`,
  `GET /ui/jobs/{id}`, `GET /ui/jobs/{id}/results` — unauthenticated twins of the `/v1` endpoints
  that the form's JS uses (also token-gated when `ui_require_token = true`).

## Public deployment (behind a reverse proxy)

The server speaks plain HTTP and expects TLS + front-door concerns to be handled by a reverse
proxy (Caddy / Traefik). Bind `endpoint` to `127.0.0.1:PORT` and proxy to it.

- **Fair scheduling** — the job queue is not strict FIFO: it round-robins across client IPs and
  caps concurrent running jobs per IP (`max_jobs_per_ip`), so one client can't starve the pool.
- **Throttling** — a per-IP token-bucket limits submit rate (`rate_per_min` / `rate_burst`),
  returning `429` past the limit. (You may also rate-limit at the proxy.)
- **Client IP** — fairness and throttling key on the client IP taken from the **rightmost**
  `X-Forwarded-For` entry, but only when the peer is in `trusted_proxies` (otherwise the header is
  ignored). Set `trusted_proxies` to your proxy's address so clients can't spoof it.
- **Tool amplification** — `type="tool"` sources (VEP/ANNOVAR spawn containers) are the main abuse
  vector on an open service; they're blocked for unauthenticated requests unless `allow_tools_unauth`.
- **Retention** — terminal jobs (and their inputs/results) are garbage-collected after `job_ttl`.
- **Upload size** — the VCF upload cap is 64 MiB; set the proxy's request-body limit ≥ that
  (Caddy `request_body { max_size 64MB }`, Traefik `buffering` middleware `maxRequestBodyBytes`).
- **Health** — point the proxy's health check at `GET /healthz`.

## Example

```sh
TOKEN=$(cganno server | head -1)          # capture the startup token (run in the background)
BASE=http://127.0.0.1:8080

# discover available annotations
curl -s -H "Authorization: Bearer $TOKEN" $BASE/v1/annotations

# submit a locus, get a job id, poll, fetch results
JID=$(curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
      -d '{"locus":"chr1:115256529:T:C","annotations":"all"}' $BASE/v1/annotate | jq -r .job_id)
curl -s -H "Authorization: Bearer $TOKEN" $BASE/v1/jobs/$JID            # {"status":"done",…}
curl -s -H "Authorization: Bearer $TOKEN" $BASE/v1/jobs/$JID/results    # [ {…} ]

# …or wait for a fast job and get results in one call (falls back to a job id if slow)
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"locus":"chr1:115256529:T:C","annotations":"all"}' "$BASE/v1/annotate?wait=10"
# → {"job_id":"…","status":"done","n_variants":1,"results":[ {…} ]}

# annotate a whole VCF (all annotations)
curl -s -H "Authorization: Bearer $TOKEN" \
     -F vcf=@variants.vcf -F annotations=all $BASE/v1/annotate/vcf
```

← Back to the **[docs index](README.md)**.
