
# syntax=docker/dockerfile:1

FROM golang:1.17-alpine as base
WORKDIR /app
COPY go.* ./
RUN go mod download -x

FROM base AS build
ENV CGO_ENABLED=0
WORKDIR /app

COPY . /app
RUN go mod tidy
RUN go build -o dist/gcs-proxy_amd64_linux ./cmd/gcsproxy

FROM alpine:3.14
COPY --from=build /app/dist/gcs-proxy_amd64_linux /usr/local/bin/gcsproxy
RUN chmod +x /usr/local/bin/gcsproxy

CMD ["gcsproxy"]