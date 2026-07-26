# varhub build management.
#
# Pure-Go (modernc.org/sqlite + hts) ⇒ CGO_ENABLED=0 cross-compiles cleanly.
# GOWORK=off is required: the parent go.work references modules that don't exist.

BIN     := varhub
PKG     := ./cmd/varhub
VERSION := $(shell v=$$(git describe --tags --always 2>/dev/null || echo dev); if ! git diff --quiet HEAD 2>/dev/null; then v=$$(echo "$$v" | sed 's/-g/-dev-g/'); echo "$$v" | grep -q -- -dev || v="$$v-dev"; fi; echo "$$v")
LDFLAGS := -s -w -X main.version=$(VERSION)
GO      := GOWORK=off CGO_ENABLED=0 go

# Release matrix. windows/amd64 is intentionally NOT in the default `cross`
# (see the separate `dist-windows` target).
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.DEFAULT_GOAL := all
.PHONY: all build test vet fmt tidy clean install cross cross-bin dist dist-windows help $(PLATFORMS)

## all: build binaries for all platforms (linux,darwin x amd64,arm64) into bin/
all:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $(BIN).$${os}_$${arch}"; \
		GOWORK=off CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -ldflags '$(LDFLAGS)' -o bin/$(BIN).$${os}_$${arch} $(PKG); \
	done

## build: compile for the host platform into bin/
build:
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/$(BIN) $(PKG)

## test: run the test suite with the race detector
test:
	GOWORK=off go test -race ./...

## vet: run go vet
vet:
	GOWORK=off go vet ./...

## fmt: format all Go files
fmt:
	gofmt -w .

## tidy: tidy go.mod
tidy:
	GOWORK=off go mod tidy

## install: install varhub into GOBIN
install:
	$(GO) install -ldflags '$(LDFLAGS)' $(PKG)

## cross: build release tarballs for linux,darwin x amd64,arm64
cross: $(PLATFORMS)
dist: cross

## cross-bin: build raw release binaries (no archive) for linux,darwin x amd64,arm64
# Emits dist/varhub-<version>-<os>-<arch> (the bare executable) — used by the
# release workflow, which attaches the raw binaries to the GitHub Release.
cross-bin:
	@mkdir -p dist; \
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		name=$(BIN)-$(VERSION)-$$os-$$arch; \
		echo "building $$name"; \
		GOWORK=off CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -ldflags '$(LDFLAGS)' -o dist/$$name $(PKG); \
	done

# One target per platform: build a static binary and tar it.
$(PLATFORMS):
	@os=$$(echo $@ | cut -d/ -f1); arch=$$(echo $@ | cut -d/ -f2); \
	name=$(BIN)-$(VERSION)-$$os-$$arch; \
	echo "building $$name"; \
	mkdir -p dist/$$name; \
	GOWORK=off CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
		go build -ldflags '$(LDFLAGS)' -o dist/$$name/$(BIN) $(PKG); \
	tar -C dist -czf dist/$$name.tar.gz $$name; \
	rm -rf dist/$$name

## dist-windows: build a windows/amd64 zip (opt-in; not built by default)
dist-windows:
	@name=$(BIN)-$(VERSION)-windows-amd64; \
	mkdir -p dist/$$name; \
	GOWORK=off CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
		go build -ldflags '$(LDFLAGS)' -o dist/$$name/$(BIN).exe $(PKG); \
	(cd dist && zip -qr $$name.zip $$name); \
	rm -rf dist/$$name

## clean: remove build artifacts
clean:
	rm -rf bin dist

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
