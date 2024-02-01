BIN_NAME := gcsproxy
GOFILES := $(shell find . -type f -name '*.go')

build: bin/$(BIN_NAME)

bin/$(BIN_NAME): $(GOFILES)
	CGO_ENABLED=0 go build -o $@ .

build-cross: clean
	GOOS=linux  GOARCH=amd64 go build -o dist/$(BIN_NAME)_amd64_linux
	GOOS=darwin GOARCH=amd64 go build -o dist/$(BIN_NAME)_amd64_darwin
	GOOS=linux  GOARCH=arm go build -o dist/$(BIN_NAME)_arm_linux
	GOOS=darwin GOARCH=arm go build -o dist/$(BIN_NAME)_arm_darwin

clean:
	rm -rf bin dist

deps:
	go get .
	go mod tidy

test:
	go test -race -v ./...
