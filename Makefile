# Build, test and release the CLI. Mirrors agent/Makefile: no toolchain
# beyond Go, and the version is stamped in rather than committed.

BINARY  := vallic
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# The platforms a customer runs this on. Windows is absent deliberately:
# every command that reaches inside an environment shells out to ssh and
# rsync, which is fine on modern Windows and worth not discovering by
# shipping. See docs/cli.md in the vallic repository.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: build test vet fmt check smoke dist clean install

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BINARY) ./cmd/$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# What CI runs. gofmt is checked rather than applied: a formatting fix
# belongs in the commit that caused it, not in a release build.
check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt:"; gofmt -l .; exit 1; }
	go vet ./...
	go test ./...

# Drives the built binary against a real control plane, which is the only
# thing that has ever caught the bugs that matter here: every one of them was
# a disagreement between what this client assumed and what the platform sends,
# and a test written from the same assumption as the client agrees with it.
#
# Read-only. Nothing it runs deploys, restores, backs up, claims a hostname or
# writes a variable.
#
#   VALLIC_API=https://vallic.ddev.site VALLIC_TOKEN=vcp_... make smoke
#
# Skips every test, loudly, without both. A DDEV site is a perfectly good
# target and is what this was written against.
smoke:
	go test -tags smoke -count=1 -v ./internal/smoke/

# One static binary per platform, with a checksum beside it. A published
# version without a checksum is the dangerous half-configured state — the
# agent's release manifest enforces the same rule at read time, and
# self-update will read this one.
dist: check
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=dist/$(BINARY)-$$os-$$arch; \
		echo "$$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/$(BINARY) || exit 1; \
	done
	@cd dist && sha256sum $(BINARY)-* > SHA256SUMS
	@echo "wrote dist/SHA256SUMS"

install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/$(BINARY)

clean:
	rm -rf dist
