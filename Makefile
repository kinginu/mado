GO      ?= $(HOME)/.local/go-sdk/go/bin/go
GOFMT   ?= $(dir $(GO))gofmt
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64 darwin/amd64

.PHONY: build test vet check darwin release clean check-deploy

build:
	$(GO) build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/mado ./cmd/mado

test:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...
	@out="$$($(GOFMT) -l cmd internal)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

check: vet test darwin

# Build-only: darwin keeps compiling (the tmux and daemon tests run on linux only).
darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build ./...

# Static binaries (no cgo: modernc.org/sqlite is pure Go) plus checksums in dist/.
release:
	rm -rf dist && mkdir -p dist
	@set -e; for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; out=dist/mado-$(VERSION)-$$os-$$arch; \
	  echo "build $$out"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/mado; \
	done
	cd dist && sha256sum mado-* > SHA256SUMS && cat SHA256SUMS

clean:
	rm -rf bin dist

check-deploy:
	bash deploy/check-deploy.sh
