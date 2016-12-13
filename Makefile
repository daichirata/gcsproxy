VERSION  := 0.1.0
BIN_NAME := bin/gcsproxy

GOFILES_NOVENDOR    := $(shell find . -type f -name '*.go' -not -path "*/vendor/*")
GOPACKAGES_NOVENDOR := $(shell glide nv)

all: $(BIN_NAME)

$(BIN_NAME): $(GOFILES_NOVENDOR)
	go build -o $@ .

build-cross: $(GOFILES_NOVENDOR)
	GOOS=linux  GOARCH=amd64 go build -o bin/linux/amd64/$(BIN_NAME)-$(VERSION)/$(BIN_NAME) .
	GOOS=darwin GOARCH=amd64 go build -o bin/darwin/amd64/$(BIN_NAME)-$(VERSION)/$(BIN_NAME) .

dist: build-cross
	cd bin/linux/amd64  && tar zcvf $(BIN_NAME)-linux-amd64-$(VERSION).tar.gz  $(BIN_NAME)-$(VERSION)
	cd bin/darwin/amd64 && tar zcvf $(BIN_NAME)-darwin-amd64-$(VERSION).tar.gz $(BIN_NAME)-$(VERSION)

deps:
	glide install

fmt:
	gofmt -l -w $(GOFILES_NOVENDOR)

simplify:
	gofmt -l -s -w $(GOFILES_NOVENDOR)

clean:
	go clean -i $(GOPACKAGES_NOVENDOR)
