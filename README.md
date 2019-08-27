# gcsproxy
Reverse proxy for Google Cloud Storage.

## Description
This is a reverse proxy for Google Cloud Storage for performing limited disclosure (IP address restriction etc...). Gets the URL of the GCS object through its internal API. Therefore, it is possible to make GCS objects private and deliver limited content.

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

## Useage

```
Usage of gcsproxy:
  -b string
    	Bind address (default "127.0.0.1:8080")
  -c string
    	The path to the keyfile. If not present, client will use your default application credentials.
  -v	Show access log

```

**Dockerfile example**

``` dockerfile
FROM alpine:3.7

ENV GCSPROXY_VERSION=0.3.0
RUN apk add --no-cache --virtual .build-deps ca-certificates wget \
  && update-ca-certificates \
  && wget https://github.com/daichirata/gcsproxy/releases/download/v${GCSPROXY_VERSION}/gcsproxy_${GCSPROXY_VERSION}_amd64_linux -O /usr/local/bin/gcsproxy \
  && chmod +x /usr/local/bin/gcsproxy \
  && apk del .build-deps

CMD ["gcsproxy"]
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
