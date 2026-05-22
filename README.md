# gcsproxy

[![Test](https://github.com/daichirata/gcsproxy/actions/workflows/test.yaml/badge.svg)](https://github.com/daichirata/gcsproxy/actions/workflows/test.yaml)
[![Release](https://img.shields.io/github/v/release/daichirata/gcsproxy)](https://github.com/daichirata/gcsproxy/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/daichirata/gcsproxy)](go.mod)
[![License](https://img.shields.io/github/license/daichirata/gcsproxy)](LICENSE)

A lightweight reverse proxy for Google Cloud Storage.

gcsproxy lets you keep a GCS bucket private while still serving its objects over HTTP, so you can put your own access controls (IP allowlist, basic auth, IAP, etc.) in front of it. It authenticates to GCS using the host's credentials and streams object contents back to the client.

```
+-----------------------------+         +------------+         +------------------+
|           Nginx             |  HTTP   |            |  GCS    |                  |
|  (auth / IP allow / TLS)    | ------> |  gcsproxy  | ------> |   Google Cloud   |
|                             |         |            |   API   |     Storage      |
+-----------------------------+         +------------+         +------------------+
```

## Quick start

Run gcsproxy locally with your existing [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials) (set up via `gcloud auth application-default login`):

```bash
docker run --rm -p 8080:80 \
    -e GOOGLE_APPLICATION_CREDENTIALS=/cred.json \
    -v ~/.config/gcloud/application_default_credentials.json:/cred.json \
    ghcr.io/daichirata/gcsproxy:latest -b 0.0.0.0:80
```

Then fetch a private object through the proxy:

```bash
curl http://localhost:8080/<your-bucket>/<your-object>
```

## Features

- Streams GCS objects directly to clients (no temporary files on disk)
- Forwards `Content-Type`, `Content-Language`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Last-Modified` (and `Content-Length` with `-content-length`)
- Honors `If-Modified-Since` and replies `304 Not Modified` when appropriate
- Supports `Range` requests (single range) for partial downloads / video seeking, forwarded to GCS so only the requested bytes traverse the wire
- Negotiates `Content-Encoding: gzip` when the client accepts it
- Optional default index file (`-i`) for serving static sites
- Optional fixed bucket (`-bucket`) for hosting a single bucket without exposing its name in URLs
- Optional SPA fallback (`-spa`) that returns the index file with HTTP 200 for unmatched routes
- Optional custom not-found page (`-not-found`) served with HTTP 404 for unmatched routes
- Optional `Access-Control-Allow-Origin` header (`-cors-origin`) for simple CORS use cases
- Structured logging via `log/slog` with text or JSON output (`-log-format`)
- `/_health` endpoint for liveness/readiness probes

## Installation

### Pre-built binaries

Download the latest release for your platform from the [Releases page](https://github.com/daichirata/gcsproxy/releases).

### Docker image

Multi-arch images (`linux/amd64`, `linux/arm64`) are published to the GitHub Container Registry on every release:

```bash
docker pull ghcr.io/daichirata/gcsproxy:latest
```

See the [Packages page](https://github.com/daichirata/gcsproxy/pkgs/container/gcsproxy) for all available tags.

### From source

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
  -i string
        Default index file to serve.
  -log-format string
        Log output format: text or json. (default "json")
  -log-level string
        Minimum log level: debug, info, warn, or error. (default "info")
  -not-found string
        Object served with HTTP 404 for unmatched routes.
  -spa
        SPA fallback: serve -i from the bucket root with HTTP 200 for unmatched routes.
  -v    Show access log.
  -walk-up-index
        When -i lookup misses, retry parent directories for index files before not-found handling.
```

### Routing

By default, the bucket name is taken from the first path segment:

```
/{bucket}/{object}
```

For example, with gcsproxy listening on `localhost:8080`, the GCS object `gs://test-bucket/path/to/file.txt` is served at `http://localhost:8080/test-bucket/path/to/file.txt`.

When `-bucket <name>` is set, that bucket is used for every request and the bucket name is no longer parsed from the URL:

```
/{object}
```

This is useful when gcsproxy is fronting exactly one bucket (e.g. a private static site) and you don't want the bucket name to appear in the path. It also avoids issues with URL-rewriting load balancers and Identity-Aware Proxy, where the rewritten path would otherwise leak into post-auth redirects.

### Default index file

If `-i` is set, requests that don't resolve to an object will fall back to the configured index file:

```
gcsproxy -i index.html

GET /test-bucket/foo/bar
  -> gs://test-bucket/foo/bar/index.html
```

When `-walk-up-index` is also set, gcsproxy will retry parent directories while keeping the same bucket/object prefix:

```
gcsproxy -i index.html -walk-up-index

GET /test-bucket/prefix/site/search
  -> try gs://test-bucket/prefix/site/search/index.html
  -> try gs://test-bucket/prefix/site/index.html
```

### SPA fallback

When `-spa` is set together with `-i`, any request that fails to resolve — even after the `-i` lookup — serves the configured index file from the bucket root with HTTP 200. This is the standard pattern for client-side-routed apps (React, Vue, etc.):

```
gcsproxy -i index.html -spa

GET /test-bucket/some/spa/route
  -> gs://test-bucket/index.html  (HTTP 200)
```

This mirrors [Cloudflare Workers'](https://developers.cloudflare.com/workers/static-assets/routing/single-page-application/) `not_found_handling = "single-page-application"` and [Netlify's](https://docs.netlify.com/manage/routing/redirects/rewrites-proxies/) `/* /index.html 200` rewrite. `-spa` requires `-i` to be set and is mutually exclusive with `-not-found`.

### Custom not-found page

When `-not-found <path>` is set, requests that fail to resolve serve the given object with HTTP 404 instead of the default plain-text 404. The object's `Content-Type` and other headers are forwarded as-is:

```
gcsproxy -not-found 404.html

GET /test-bucket/missing
  -> gs://test-bucket/404.html  (HTTP 404)
```

### Logging

gcsproxy uses Go's standard `log/slog` package and writes structured logs to stderr. The default `json` format is ready to be ingested by Cloud Logging, Datadog, Loki, and similar aggregation pipelines:

```json
{"time":"2026-05-21T09:00:00Z","level":"INFO","msg":"access","remote":"127.0.0.1","elapsed":0.0042,"status":200,"method":"GET","url":"/bucket/foo/bar"}
```

Pass `-log-format text` for human-readable `key=value` output when running locally:

```
gcsproxy -log-format text -v
```

The access log line is only emitted when `-v` is set.

`-log-level <debug|info|warn|error>` (default `info`) filters at the slog handler. Set it to `warn` to suppress the `INFO` startup and access logs while keeping `WARN`/`ERROR` visible:

```
gcsproxy -log-level warn
```

### Transfer encoding

By default gcsproxy does not emit the `Content-Length` header. `net/http` then uses `Transfer-Encoding: chunked` for any response large enough to matter, which bypasses the [32 MiB non-streamed response cap](https://cloud.google.com/run/quotas) enforced by platforms like Cloud Run.

Small responses may still get an auto-populated `Content-Length` from `net/http`, but that is harmless because they are well below any platform limit.

Pass `-content-length` to emit the header for every response — useful when clients need to know the total size up front for progress indicators. The 32 MiB Cloud Run cap will then apply.

### CORS

Pass `-cors-origin <value>` to add an `Access-Control-Allow-Origin` header to every response (success and error alike):

```
gcsproxy -cors-origin '*'
gcsproxy -cors-origin 'https://example.com'
```

This is sufficient for [simple cross-origin requests](https://developer.mozilla.org/en-US/docs/Web/HTTP/CORS#simple_requests) — `<img>`, `<link>`, `<video>`, plain `fetch(url)`, etc. — which is what most static-site hosting needs.

Requests that require a preflight (custom headers, credentialed `fetch`, non-`GET/HEAD/POST` methods) are not supported; front gcsproxy with a proxy like nginx if you need that.

### Range requests

gcsproxy advertises `Accept-Ranges: bytes` on full responses (except for `Content-Encoding: gzip` objects, see below) and serves a single byte range when the client sends a `Range` header:

```
GET /test-bucket/video.mp4
Range: bytes=1048576-2097151

→ 206 Partial Content
   Content-Range: bytes 1048576-2097151/<total>
   Content-Length: 1048576
```

The range is forwarded to GCS via [`NewRangeReader`](https://pkg.go.dev/cloud.google.com/go/storage#ObjectHandle.NewRangeReader), so only the requested bytes are transferred from GCS — useful for video seeking, resumable downloads, and large-file streaming.

Supported forms: `bytes=N-M`, `bytes=N-`, and `bytes=-N`. Multi-range requests (`bytes=0-99,200-299`) are reduced to their first range; `multipart/byteranges` responses are not implemented.

Edge cases:

- Range headers with a non-`bytes` unit are ignored and the request is served as a normal `200`, per [RFC 9110 §14.2](https://httpwg.org/specs/rfc9110.html#field.range). The same ignore-and-fall-through applies to byte-ranges that fail to parse as integers.
- Byte-ranges that fall outside the object — or whose last byte precedes the first (e.g. `bytes=500-100`) — return `416` with a `Content-Range: bytes */<size>` header.
- `If-Range` is not honored — the range is always served when an in-bounds `Range` header is present.
- Objects stored with `Content-Encoding: gzip` cannot be served as partial content (GCS transcodes them and offsets become ambiguous), so the handler falls back to a full `200` body.

### Health check

`/_health` returns `200 OK` with the body `OK`. It does not call GCS and is safe to use as a Kubernetes/Cloud Run liveness or readiness probe.

### Authentication

gcsproxy uses [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials) by default, so on GCE/GKE/Cloud Run it picks up the service account attached to the workload. To use a specific service-account key file, pass `-c /path/to/key.json`.

## Examples

### Docker

```bash
docker run --rm -p 8080:80 \
    -e GOOGLE_APPLICATION_CREDENTIALS=/cred.json \
    -v /path/to/key.json:/cred.json \
    ghcr.io/daichirata/gcsproxy:latest -v
```

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

```ini
[Unit]
Description=gcsproxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/gcsproxy/gcsproxy -v
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

### nginx

```nginx
upstream gcsproxy {
    server 127.0.0.1:8080;
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
    }
}
```
