# gcsproxy

[![Test](https://github.com/daichirata/gcsproxy/actions/workflows/test.yaml/badge.svg)](https://github.com/daichirata/gcsproxy/actions/workflows/test.yaml)
[![Release](https://img.shields.io/github/v/release/daichirata/gcsproxy)](https://github.com/daichirata/gcsproxy/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/daichirata/gcsproxy)](go.mod)
[![License](https://img.shields.io/github/license/daichirata/gcsproxy)](LICENSE)

**A lightweight reverse proxy for Google Cloud Storage.**

Keep a bucket private and still serve its objects over HTTP. gcsproxy authenticates to GCS with the host's credentials and streams objects to the client, so you can put any access control you like — IP allowlists, basic auth, IAP — in front of it.

```
╭────────╮       ╭───────────────────╮      ╭──────────╮         ╭───────────────╮
│ Client │ ────▶ │ nginx / LB / IAP  │ ───▶ │ gcsproxy │ ──────▶ │ Cloud Storage │
╰────────╯ HTTPS ╰───────────────────╯ HTTP ╰──────────╯ GCS API ╰───────────────╯
                  auth · IP allow · TLS                           private bucket
```

## Contents

- [Quick start](#quick-start)
- [Features](#features)
- [Installation](#installation)
- [Usage](#usage)
- [Serving](#serving) — routing, index files, SPA, not-found pages, caching, ranges, CORS
- [Operations](#operations) — authentication, health checks, graceful shutdown, h2c, logging
- [Examples](#examples) — Docker, Compose, systemd, nginx

## Quick start

Run gcsproxy with your local [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials) (`gcloud auth application-default login`):

```bash
docker run --rm -p 8080:80 \
    -e GOOGLE_APPLICATION_CREDENTIALS=/cred.json \
    -v ~/.config/gcloud/application_default_credentials.json:/cred.json \
    ghcr.io/daichirata/gcsproxy:latest -b 0.0.0.0:80
```

Then fetch a private object through it:

```bash
curl http://localhost:8080/<your-bucket>/<your-object>
```

## Features

**Serving**

- Streams objects straight from GCS — no temporary files on disk
- Forwards `Content-Type`, `Content-Language`, `Cache-Control`, `Content-Disposition`, `Content-Encoding` and `Last-Modified` (plus `Content-Length` with `-content-length`)
- Negotiates `Content-Encoding: gzip` with the client
- Optional fixed bucket (`-bucket`) that keeps the bucket name out of URLs
- Optional `Access-Control-Allow-Origin` header (`-cors-origin`) for simple CORS

**Caching and partial content**

- `ETag` plus RFC 9110 conditional requests — `If-None-Match`, `If-Modified-Since`, `If-Match`, `If-Unmodified-Since`, `If-Range` — answered with `304` / `412`
- Single-range `Range` requests for video seeking and resumable downloads; only the requested bytes leave GCS

**Static sites**

- Default index file (`-i`), optionally walking up parent directories (`-walk-up-index`)
- SPA fallback (`-spa`) that serves the index with `200` for client-side routes
- Custom not-found page (`-not-found`) served with `404`

**Operations**

- Graceful shutdown with configurable drain windows for zero-downtime deploys
- Optional cleartext HTTP/2 (`-h2c`) alongside HTTP/1.1
- `/_health` endpoint for liveness and readiness probes
- Structured logging via `log/slog`, as JSON or text
- Error responses that hide bucket and object names by default (`-redact-errors`)

## Installation

**Pre-built binaries** — download the archive for your platform from the [Releases page](https://github.com/daichirata/gcsproxy/releases).

**Docker** — multi-arch images (`linux/amd64`, `linux/arm64`) are published to the GitHub Container Registry on every release ([all tags](https://github.com/daichirata/gcsproxy/pkgs/container/gcsproxy)):

```bash
docker pull ghcr.io/daichirata/gcsproxy:latest
```

**From source**:

```bash
go install github.com/daichirata/gcsproxy@latest
```

## Usage

```
Usage of gcsproxy:
  -b string
        Bind address. (default "127.0.0.1:8080")
  -bucket string
        Fixed bucket name. Disables bucket extraction from the path.
  -c string
        Path to a service-account key file. Defaults to Application Default Credentials.
  -content-length
        Send the Content-Length header (disables chunked transfer).
  -cors-origin string
        Value for the Access-Control-Allow-Origin header.
  -h2c
        Enable cleartext HTTP/2 (h2c) with prior knowledge alongside HTTP/1.1.
  -i string
        Default index file to serve.
  -log-format string
        Log output format: text or json. (default "json")
  -log-level string
        Minimum log level: debug, info, warn, or error. (default "info")
  -not-found string
        Object served with HTTP 404 for unmatched routes.
  -redact-errors
        Replace error response bodies with the generic HTTP status text. (default true)
  -shutdown-delay duration
        Delay after SIGTERM/SIGINT during which requests are served normally but responses carry Connection: close.
  -shutdown-idle-grace-period duration
        After the listener closes, keep idle connections open for up to this long; ends early once no connections remain.
  -shutdown-timeout duration
        Max wait for in-flight requests after the idle grace period; 0 waits indefinitely. (default 30s)
  -spa
        SPA fallback: serve -i from the bucket root with HTTP 200 for unmatched routes.
  -v    Show access log.
  -walk-up-index
        When -i lookup misses, retry parent directories for index files before not-found handling.
```

## Serving

### Routing

By default the bucket name is taken from the first path segment:

```
/{bucket}/{object}

GET http://localhost:8080/test-bucket/path/to/file.txt
  → gs://test-bucket/path/to/file.txt
```

With `-bucket <name>`, every request is served from that bucket and the whole path is the object name:

```
/{object}
```

Use this when gcsproxy fronts exactly one bucket, such as a private static site. It keeps the bucket name out of URLs and avoids URL-rewriting load balancers and Identity-Aware Proxy leaking the rewritten path into post-auth redirects.

### Default index file

With `-i`, a request that doesn't resolve to an object falls back to the index file under that path:

```
gcsproxy -i index.html

GET /test-bucket/foo/bar
  → gs://test-bucket/foo/bar/index.html
```

Add `-walk-up-index` to keep trying parent directories, staying within the same bucket:

```
gcsproxy -i index.html -walk-up-index

GET /test-bucket/prefix/site/search
  → try gs://test-bucket/prefix/site/search/index.html
  → try gs://test-bucket/prefix/site/index.html
  → …
```

### SPA fallback

With `-spa` (requires `-i`), any request that still fails to resolve serves the index file from the bucket root with `200` — the standard setup for client-side-routed apps (React, Vue, …):

```
gcsproxy -i index.html -spa

GET /test-bucket/some/spa/route
  → gs://test-bucket/index.html  (200)
```

This mirrors [Cloudflare Workers'](https://developers.cloudflare.com/workers/static-assets/routing/single-page-application/) `not_found_handling = "single-page-application"` and [Netlify's](https://docs.netlify.com/manage/routing/redirects/rewrites-proxies/) `/* /index.html 200` rewrite. `-spa` and `-not-found` are mutually exclusive.

### Custom not-found page

With `-not-found <path>`, requests that fail to resolve serve that object with `404` instead of the plain-text default. The object's `Content-Type` and other headers are forwarded as-is:

```
gcsproxy -not-found 404.html

GET /test-bucket/missing
  → gs://test-bucket/404.html  (404)
```

### Conditional requests

gcsproxy emits the object's [GCS etag](https://cloud.google.com/storage/docs/hashes-etags) as `ETag` and evaluates the [RFC 9110 preconditions](https://httpwg.org/specs/rfc9110.html#preconditions) against it and `Last-Modified`:

```
GET /test-bucket/app.js

→ 200 OK
  ETag: "CJqzl4Ol0PACEAE="
  Last-Modified: Wed, 04 Mar 2026 05:06:07 GMT

GET /test-bucket/app.js
If-None-Match: "CJqzl4Ol0PACEAE="

→ 304 Not Modified
  ETag: "CJqzl4Ol0PACEAE="
  Last-Modified: Wed, 04 Mar 2026 05:06:07 GMT
  Cache-Control: <the object's Cache-Control, if any>
```

| Header | Comparison | When it fails |
|---|---|---|
| `If-None-Match` | weak; lists and `*` | `304 Not Modified` on match. Overrides `If-Modified-Since`. |
| `If-Modified-Since` | second granularity | `304 Not Modified` if not modified since. |
| `If-Match` | strong; lists and `*` | `412 Precondition Failed` on no match. |
| `If-Unmodified-Since` | second granularity | `412 Precondition Failed` if modified since. Ignored when `If-Match` is present. |
| `If-Range` | strong etag or exact date | Full `200` instead of `206` — see [Range requests](#range-requests). |

Things worth knowing:

- **The etag tracks the GCS generation, not the bytes.** Re-uploading identical content, or changing the object's metadata, produces a new `ETag`.
- **Weak `If-None-Match` comparison** lets revalidation survive intermediaries that weaken the tag: a cache that recompresses the body (nginx gzip filter, Varnish, …) turns `"abc"` into `W/"abc"`, and `If-None-Match: W/"abc"` still gets a `304`.
- **Preconditions run before `Range`**, so a matching `If-None-Match` answers `304` rather than `206`, and a failing `If-Match` answers `412`.
- **gzip-stored objects get a weak etag** (`W/"…"`) and `Vary: Accept-Encoding`: the same URL serves either the raw gzip bytes or the GCS-transcoded body depending on `Accept-Encoding`.
- **Index, walk-up and SPA responses** revalidate against the resolved index object, so clients can cache the app shell. The `-not-found` page is never conditional.
- **Invalid input is ignored, as in `net/http`**: unparsable dates, malformed entity-tags and empty `If-Match`/`If-None-Match` fields don't affect the response. All three HTTP-date formats are accepted.
- **Reads are pinned to the validated generation**, so a concurrent overwrite can never pair a new body with stale validators. If that generation disappears mid-request, the request is re-evaluated once against the new one — preconditions and range included — and answers `503` if it changes again.

### Range requests

Full responses advertise `Accept-Ranges: bytes`, and a single byte range is served when the client asks for one:

```
GET /test-bucket/video.mp4
Range: bytes=1048576-2097151

→ 206 Partial Content
  Content-Range: bytes 1048576-2097151/<total>
  Content-Length: 1048576
```

The range is forwarded to GCS via [`NewRangeReader`](https://pkg.go.dev/cloud.google.com/go/storage#ObjectHandle.NewRangeReader), so only the requested bytes are transferred — ideal for video seeking, resumable downloads and large files.

Supported forms are `bytes=N-M`, `bytes=N-` and `bytes=-N`. Multi-range requests (`bytes=0-99,200-299`) are reduced to their first range; `multipart/byteranges` is not implemented.

| Request | Response |
|---|---|
| In-bounds range | `206` with `Content-Range` |
| Out-of-bounds range, or last byte before first (`bytes=500-100`) | `416` with `Content-Range: bytes */<size>` |
| Non-`bytes` unit or unparsable range ([RFC 9110 §14.2](https://httpwg.org/specs/rfc9110.html#field.range)) | Range ignored, full `200` |
| `If-Range` doesn't match (weak tags never do) | Range ignored, full `200` — a resumed download never mixes two versions |
| Object stored with `Content-Encoding: gzip` | Full `200` — GCS transcodes these, so offsets would be ambiguous |

### Transfer encoding

By default gcsproxy omits `Content-Length`, so `net/http` streams anything sizeable with `Transfer-Encoding: chunked`. That sidesteps the [32 MiB non-streamed response cap](https://cloud.google.com/run/quotas) of platforms like Cloud Run. (Small responses may still get an automatic `Content-Length`, which is harmless.)

Pass `-content-length` when clients need the total size up front, e.g. for progress bars — the Cloud Run cap then applies.

### CORS

`-cors-origin <value>` adds `Access-Control-Allow-Origin` to every response, successful or not:

```
gcsproxy -cors-origin '*'
gcsproxy -cors-origin 'https://example.com'
```

That covers [simple cross-origin requests](https://developer.mozilla.org/en-US/docs/Web/HTTP/CORS#simple_requests) — `<img>`, `<link>`, `<video>`, plain `fetch(url)` — which is what static hosting usually needs. Preflighted requests (custom headers, credentialed `fetch`, methods other than `GET`/`HEAD`/`POST`) are not supported; put a proxy such as nginx in front if you need them.

## Operations

### Authentication

gcsproxy uses [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials), so on GCE, GKE and Cloud Run it picks up the workload's service account automatically. Pass `-c /path/to/key.json` to use a specific service-account key instead. The account only needs read access to the objects (e.g. `roles/storage.objectViewer`).

### Health check

`/_health` returns `200 OK` with the body `OK`. It never calls GCS, so it is safe for Kubernetes and Cloud Run liveness or readiness probes, and it keeps answering `200` during graceful shutdown so a liveness probe won't kill the process mid-drain.

### Graceful shutdown

On SIGTERM or SIGINT, gcsproxy shuts down in four phases designed to avoid 502/503s and connection resets during rolling deploys:

1. **Drain** (`-shutdown-delay`) — new connections are still accepted and requests served normally, but every response carries `Connection: close`, telling keep-alive clients (e.g. an nginx `upstream keepalive` pool) to retire the connection.
2. **Stop accepting** — the listener closes and new TCP connections are refused. Existing connections keep working.
3. **Idle grace** (`-shutdown-idle-grace-period`) — idle connections stay open, because closing one while a client sends on it is exactly what causes resets. Requests on them are still served, each marked `Connection: close`. The phase ends as soon as no connections remain.
4. **Close** (`-shutdown-timeout`) — remaining idle connections are closed and in-flight requests get up to `-shutdown-timeout` (default `30s`; `0` waits forever) to finish. Then gcsproxy exits 0.

With the defaults (`0s` delay and grace), SIGTERM still waits for in-flight requests before exiting. Recommended production values:

```
gcsproxy -shutdown-delay 5s -shutdown-idle-grace-period 5s -shutdown-timeout 30s
```

Sizing guidance:

- Set `-shutdown-idle-grace-period` above your clients' idle-connection timeout (nginx upstream `keepalive_timeout`, Go's `Transport.IdleConnTimeout`, …), so every pooled connection is either used once more and closed cleanly, or closed by the client first.
- Keep your supervisor's kill timeout (systemd `TimeoutStopSec`, Kubernetes `terminationGracePeriodSeconds`) above `shutdown-delay + shutdown-idle-grace-period + shutdown-timeout`, or the process is SIGKILLed mid-drain.
- A second SIGTERM/SIGINT terminates immediately.

### HTTP/2 cleartext (h2c)

`-h2c` accepts HTTP/2 over cleartext TCP alongside HTTP/1.1 on the same address. Use it when a TLS-terminating load balancer forwards HTTP/2 without re-encrypting, such as Cloud Run with [HTTP/2 end-to-end](https://cloud.google.com/run/docs/configuring/http2).

Only prior-knowledge h2c is supported: the client must open with the HTTP/2 preface. `Upgrade: h2c` requests are served over HTTP/1.1. During graceful shutdown, HTTP/2 connections receive `GOAWAY` instead of `Connection: close`.

### Logging

gcsproxy writes structured logs to stderr via `log/slog`. The default `json` format is ready for Cloud Logging, Datadog, Loki and friends:

```json
{"time":"2026-05-21T09:00:00Z","level":"INFO","msg":"access","remote":"127.0.0.1","elapsed":0.0042,"status":200,"method":"GET","url":"/bucket/foo/bar"}
```

- `-v` enables the access log shown above.
- `-log-format text` switches to human-readable `key=value` output for local runs.
- `-log-level <debug|info|warn|error>` (default `info`) sets the minimum level; `warn` hides the startup and access logs.

Failed requests are logged as `proxy error` with the status, URL and full GCS error:

```json
{"time":"2026-05-21T09:00:00Z","level":"ERROR","msg":"proxy error","status":500,"url":"/bucket/foo/bar","err":"googleapi: Error 403: ..."}
```

Server errors are logged at `ERROR`, `503`s from repeated concurrent overwrites at `WARN`, and missing objects (`404`) and client cancellations at `DEBUG`.

### Error responses

GCS error messages can reveal bucket and object names, or details about the service account's permissions. By default the response body carries only the status text, and the full error goes to the [log](#logging):

```
GET /test-bucket/missing
  → 404, body "Not Found"
```

Pass `-redact-errors=false` to return the GCS error in the body, e.g. while debugging locally.

## Examples

### Docker

```bash
docker run --rm -p 8080:80 \
    -e GOOGLE_APPLICATION_CREDENTIALS=/cred.json \
    -v /path/to/key.json:/cred.json:ro \
    ghcr.io/daichirata/gcsproxy:latest -b 0.0.0.0:80 -v
```

Arguments replace the image's default command (`-b 0.0.0.0:80`), so keep `-b 0.0.0.0:80` when adding flags.

### Docker Compose

```yaml
services:
  gcsproxy:
    image: ghcr.io/daichirata/gcsproxy:latest
    restart: unless-stopped
    ports:
      - "8080:80"
    command: -b 0.0.0.0:80 -v
    volumes:
      - ./key.json:/cred.json:ro
    environment:
      GOOGLE_APPLICATION_CREDENTIALS: /cred.json
```

### systemd

A ready-to-use unit is included as [`gcsproxy.service`](gcsproxy.service):

```ini
[Unit]
Description=gcsproxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/gcsproxy/gcsproxy -v -shutdown-delay 5s -shutdown-idle-grace-period 5s -shutdown-timeout 30s
Restart=on-failure
# Must exceed shutdown-delay + shutdown-idle-grace-period + shutdown-timeout.
TimeoutStopSec=45

[Install]
WantedBy=multi-user.target
```

### nginx

```nginx
upstream gcsproxy {
    server 127.0.0.1:8080;
    keepalive 16;
}

server {
    listen 8081;
    server_name _;

    access_log off;
    error_log /var/log/nginx/gcsproxy.error.log error;

    if ($request_method !~ "GET|HEAD") {
        return 405;
    }

    location / {
        proxy_pass http://gcsproxy;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
    }
}
```

`keepalive` with HTTP/1.1 and an empty `Connection` header lets nginx reuse upstream connections; gcsproxy's [graceful shutdown](#graceful-shutdown) retires them cleanly during deploys.

## License

[MIT](LICENSE)
