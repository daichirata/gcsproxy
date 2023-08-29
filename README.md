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

## Usage

```
Usage of gcsproxy:
  -b string
    	Bind address (default "127.0.0.1:8080")
  -c string
    	The path to the keyfile. If not present, client will use your default application credentials.
  -v	Show access log

```

The gcsproxy routing configuration is shown below.

`"/{bucket:[0-9a-zA-Z-_.] +}/{object:. *}"`

If you are running gcsproxy on localhost:8080 and you want to access the file `gs://test-bucket/your/file/path.txt` in GCS via gcsproxy,
you can use the URL You can access the file via gcsproxy at the URL `http://localhost:8080/test-bucket/your/file/path.txt`.

## Configurations

**Dockerfile example**

``` dockerfile
FROM debian:buster-slim AS build

WORKDIR /tmp
ENV GCSPROXY_VERSION=0.3.1

RUN apt-get update \
    && apt-get install --no-install-suggests --no-install-recommends --yes ca-certificates wget \
    && wget https://github.com/daichirata/gcsproxy/releases/download/v${GCSPROXY_VERSION}/gcsproxy-${GCSPROXY_VERSION}-linux-amd64.tar.gz \
    && tar zxf gcsproxy-${GCSPROXY_VERSION}-linux-amd64.tar.gz \
    && cp ./gcsproxy-${GCSPROXY_VERSION}-linux-amd64/gcsproxy .

FROM gcr.io/distroless/base
COPY --from=build /tmp/gcsproxy /gcsproxy
CMD ["/gcsproxy"]
```

### Docker image build example

```bash
docker build --build-arg GCSPROXY_VERSION=0.4.0 -t gcsproxy .
```

Example how to run the image

The **d53ee11da87c.json** JSON files contains the Google Cloud Service Account credentials.

```bash
docker run \
    -it --rm \
    -p 8080:80 \
    -e GOOGLE_APPLICATION_CREDENTIALS=/cred.json \
    -v $(pwd)/../d53ee11da87c.json:/cred.json gcsproxy 
```

### Docker Compose example

```dockerfile
version: '3.3'

networks:
  web:

services:
  gcsproxy:
    build:
      context: .
      dockerfile: Dockerfile
      args:
        GCSPROXY_VERSION: 0.4.0
        #HTTPS_PROXY: http://192.168.1.1:8080/
        #HTTP_PROXY: http://192.168.1.1:8080/
    restart: unless-stopped
    networks:
      - "web"
    ports:
      - 8080:80
    command: -b 0.0.0.0:80 -c /cred.json
    #environment:
      #HTTPS_PROXY: http://192.168.1.1:8080/
      #HTTP_PROXY: http://192.168.1.1:8080/
    volumes:
      - ./d53ee11da87c.json:/cred.json
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
