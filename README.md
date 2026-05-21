# gcsproxy

A lightweight reverse proxy for Google Cloud Storage.

gcsproxy lets you keep a GCS bucket private while still serving its objects over HTTP, so you can put your own access controls (IP allowlist, basic auth, IAP, etc.) in front of it. It authenticates to GCS using the host's credentials and streams object contents back to the client.

```
+-----------------------------+         +------------+         +------------------+
|           Nginx             |  HTTP   |            |  GCS    |                  |
|  (auth / IP allow / TLS)    | ------> |  gcsproxy  | ------> |   Google Cloud   |
|                             |         |            |   API   |     Storage      |
+-----------------------------+         +------------+         +------------------+
```

## Features

- Streams GCS objects directly to clients (no temporary files on disk)
- Forwards `Content-Type`, `Content-Language`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Last-Modified` (and `Content-Length` with `-content-length`)
- Honors `If-Modified-Since` and replies `304 Not Modified` when appropriate
- Negotiates `Content-Encoding: gzip` when the client accepts it
- Optional default index file (`-i`) for serving static sites
- Optional fixed bucket (`-bucket`) for hosting a single bucket without exposing its name in URLs
- Optional SPA fallback (`-spa`) that returns the index file with HTTP 200 for unmatched routes
- Optional custom not-found page (`-not-found`) served with HTTP 404 for unmatched routes
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
        Bind address (default "127.0.0.1:8080")
  -c string
        The path to the keyfile. If not present, client will use your default application credentials.
  -bucket string
        Fixed bucket name. If unset, the bucket is taken from the first path segment.
  -content-length
        Send the Content-Length header. By default it is omitted so responses use Transfer-Encoding: chunked, which avoids platform response-size limits (e.g. Cloud Run's 32 MiB cap).
  -i string
        The default index file to serve.
  -log-format string
        Log output format: text or json. (default "json")
  -not-found string
        Object path served with HTTP 404 when no object matches the request. Mutually exclusive with -spa.
  -spa
        Single-page application fallback. When a request does not match an object, serve the -i index file from the bucket root with HTTP 200. Requires -i; mutually exclusive with -not-found.
  -v    Show access log
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

### Transfer encoding

By default gcsproxy does not emit the `Content-Length` header. `net/http` then uses `Transfer-Encoding: chunked` for any response large enough to matter — which is what platforms like Cloud Run need to bypass their [32 MiB non-streamed response cap](https://cloud.google.com/run/quotas). Small responses may still get an auto-populated `Content-Length` from `net/http`, but that is harmless because they are well below any platform limit.

Pass `-content-length` to opt back into emitting the header for every response, e.g. when clients need to know the total size up front for progress indicators. With this flag, the 32 MiB Cloud Run cap will apply.

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
