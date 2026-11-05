BIN_NAME := gcsproxy
GOFILES := $(shell find . -type f -name '*.go')

.PHONY: build test vet tidy clean

build: bin/$(BIN_NAME)

bin/$(BIN_NAME): $(GOFILES) go.mod go.sum
	go build -o $@ .

test:
	go test -race ./...

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin dist
