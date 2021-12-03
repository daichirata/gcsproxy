[![Go](https://github.com/mike-sirs/gcsproxy/actions/workflows/go.yml/badge.svg?branch=master)](https://github.com/mike-sirs/gcsproxy/actions/workflows/go.yml)

# gcsproxy
Reverse proxy for Google Cloud Storage.

## New

Added:
  - opentelemetry:
  ```
  to enable use flag `-otel true`
  env vars:
  `OTEL_EXPORTER_OTLP_ENDPOINT` to set exporter endpoint
  `OTEL_EXPORTER_OTLP_SECURE` set to `true` or `1` or else if you use TLS on your `otel` ingress. (Default insecure.)
  `GO_GORILLA_SERVICE_NAME` the service name used to display traces
```

## Description
This is a reverse proxy for Google Cloud Storage for performing limited disclosure (IP address restriction etc...). Gets the URL of the GCS object through its internal API. Therefore, it is possible to make GCS objects private and deliver limited content.

## Changes
Difference from daichirata/gcsproxy is that it's possible to put a static site in a private GCS bucket behind GLB with IAP.

New.
- run in CloudRun
- Pull SA key from secretManager
- redirect 404 to index.html
- set index page like index.html
- use host name as a bucket name

Redirect pattern:
`try_files $uri $uri/index.html /index.html`

Request flow: 
```
User ==(https)> GlobalLB with IAP enabled ==(http)> CloudRun GCS-proxy ==(https)> GCS Private bucket
```
- Bucket name must be the same as a DNS name since we use the hostname as a key to addressing the right bucket.

```
 +---------------------------------------+
 |                Nginx                  |
 |    access control (basic auth/ip)     |
 +-----+---------------------------------+
       |
-----------------------------------------+
       |
       |
+------v-----+          +---------------+
|            |          |               |
|  gcsproxy  | +------> | Google Cloud  |
|            |          |    Storage    |
+------------+          +---------------+
```

## Usage

```
Usage of gcsproxy:
  -b string
     Bind address. (default "127.0.0.1:8080")
  -c string
     The path to the keyfile. If not present, client will use your default application credentials.
  -i string
     Index page file name.
  -dn Use hostname as a bucket name.
  -otel bool
     Enable opentelemetry. (default false)
  -r Redirect to index.html if 404 not found.
  -s string
     Use SA key from secretManager. E.G. 'projects/937121755211/secrets/gcs-proxy/versions/1'
  -v Show access log.

```

**systemd example**

```
[Unit]
Description=gcsproxy

[Service]
Type=simple
ExecStart=/opt/gcsproxy/gcsproxy -v
ExecStop=/bin/kill -SIGTERM $MAINPID

[Install]
WantedBy = multi-user.target
```

**nginx.conf**

```
upstream gcsproxy {
    server '127.0.0.1:8080';
}

server {
    listen 8081;
    server_name _;

    # Logs
    access_log off;
    error_log /var/log/nginx/gcsproxy.error.log error;

    if ($request_method !~ "GET|HEAD|PURGE") {
        return 405;
    }

    location / {
        proxy_pass http://gcsproxy$uri;
    }
}
```
