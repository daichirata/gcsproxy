# syntax=docker/dockerfile:1
FROM golang:1.18 AS builder

WORKDIR /go/src/gcsproxy
COPY go.mod .
COPY go.sum .
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go get -d -v ./...
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build .

FROM ubuntu:20.04
RUN addgroup -q gcsproxy --gid 1000 && \
    adduser -q gcsproxy --uid 1000 --gid 1000 \
      --disabled-password --disabled-login \
      --gecos "GCSProxy user"

COPY --from=builder /go/src/gcsproxy/gcsproxy /usr/local/bin/gcsproxy

EXPOSE 8080

USER=gcsproxy
CMD ["/usr/local/bin/gcsproxy", "-b", "0.0.0.0:8080"]
