BINARY=q8s
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: all build install test e2e vet lint fmt clean

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

# Same checks CI runs: gofmt cleanliness + staticcheck. Falls back to a
# warning if staticcheck is not installed locally.
lint:
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; fi
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "note: staticcheck not installed (go install honnef.co/go/tools/cmd/staticcheck@2026.2.1)"; fi

fmt:
	go fmt ./...

clean:
	rm -f $(BINARY)
