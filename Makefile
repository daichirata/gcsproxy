VERSION  := 0.3.0
BIN_NAME := gcsproxy

GOFILES_NOVENDOR    := $(shell find . -type f -name '*.go' -not -path "*/vendor/*")
GOPACKAGES_NOVENDOR := $(shell glide nv)

all: bin/$(BIN_NAME)

bin/$(BIN_NAME): $(GOFILES_NOVENDOR)
	go build -o $@ .

build-cross: $(GOFILES_NOVENDOR)
	GOOS=linux  GOARCH=amd64 go build -o dist/$(BIN_NAME)_$(VERSION)_amd64_linux
	GOOS=darwin GOARCH=amd64 go build -o dist/$(BIN_NAME)_$(VERSION)_amd64_darwin

deps:
	glide install

fmt:
	gofmt -l -w $(GOFILES_NOVENDOR)

simplify:
	gofmt -l -s -w $(GOFILES_NOVENDOR)

clean:
	go clean -i $(GOPACKAGES_NOVENDOR)
