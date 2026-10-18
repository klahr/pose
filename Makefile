BINARY  := pose
PREFIX  ?= $(HOME)/.local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

export CGO_ENABLED := 0

.PHONY: all build test vet fmt install uninstall clean

all: vet test build

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/pose

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

install: build
	install -Dm755 bin/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)

clean:
	rm -rf bin
