FROM golang:1.21 as builder

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY main.go main.go

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -a -o gcsproxy main.go

# Use from scratch
FROM scratch
WORKDIR /
COPY --from=builder /workspace/gcsproxy /
ENTRYPOINT ["/gcsproxy"]
CMD [ "-b", "0.0.0.0:80" ]
