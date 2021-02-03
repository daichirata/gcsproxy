FROM golang:1.14-alpine as build

WORKDIR /app
COPY . /app
RUN go build -o dist/gcs-proxy_amd64_linux

FROM alpine:3.13
COPY --from=build /app/dist/gcs-proxy_amd64_linux /usr/local/bin/gcsproxy
RUN chmod +x /usr/local/bin/gcsproxy

CMD ["gcsproxy"]