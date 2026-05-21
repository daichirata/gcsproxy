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
- Forwards `Content-Type`, `Content-Language`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Length`, `Last-Modified`
- Honors `If-Modified-Since` and replies `304 Not Modified` when appropriate
- Negotiates `Content-Encoding: gzip` when the client accepts it
- Optional default index file (`-i`) for serving static sites
- Optional fixed bucket (`-bucket`) for hosting a single bucket without exposing its name in URLs
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
  -i string
        The default index file to serve.
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
