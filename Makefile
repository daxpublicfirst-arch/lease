# lease — Makefile
#
# Static binary, no external Go dependencies (stdlib only).

BINARY := lease
VERSION := 0.1.0
DIST := dist

.PHONY: all build test vet self-test doctor clean dist sha256

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/$(BINARY) ./cmd/lease

test:
	go test ./...

vet:
	go vet ./...
	gofmt -l cmd internal

self-test: build
	./bin/$(BINARY) --self-test

doctor: build
	./bin/$(BINARY) doctor

clean:
	rm -rf bin $(DIST)

dist: clean
	mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o $(DIST)/linux-amd64/bin/$(BINARY) ./cmd/lease
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o $(DIST)/linux-arm64/bin/$(BINARY) ./cmd/lease
	cp plugin.toml README.md LICENSE CHANGELOG.md $(DIST)/linux-amd64/
	cp plugin.toml README.md LICENSE CHANGELOG.md $(DIST)/linux-arm64/
	tar -C $(DIST)/linux-amd64 -czf $(DIST)/lease-$(VERSION)-linux-amd64.tar.gz bin/$(BINARY) plugin.toml README.md LICENSE CHANGELOG.md
	tar -C $(DIST)/linux-arm64 -czf $(DIST)/lease-$(VERSION)-linux-arm64.tar.gz bin/$(BINARY) plugin.toml README.md LICENSE CHANGELOG.md

sha256: dist
	cd $(DIST) && sha256sum lease-$(VERSION)-linux-amd64.tar.gz lease-$(VERSION)-linux-arm64.tar.gz linux-amd64/bin/$(BINARY) linux-arm64/bin/$(BINARY) > sha256sum.txt
	cat $(DIST)/sha256sum.txt
