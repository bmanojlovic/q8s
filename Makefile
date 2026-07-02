BINARY=q8s
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: all build install test e2e vet clean

all: vet build

build: $(BINARY)

GO_SOURCES := $(shell find . -name '*.go' -not -path './.git/*')

$(BINARY): $(GO_SOURCES)
	go build $(LDFLAGS) -o $@ ./cmd/q8s/

install: build
	./$(BINARY) install

test:
	go test -v -cover ./...

e2e: build
	go build $(LDFLAGS) -o /tmp/q8s-e2e ./cmd/q8s/
	uv run --with pexpect e2e_test.py

vet:
	go vet ./...

fmt:
	go fmt ./...

clean:
	rm -f $(BINARY)
