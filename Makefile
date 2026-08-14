BINARY := containerd-shim-crawlc-v1
PREFIX ?= /usr/local
GO ?= go

# Build for the host unless asked otherwise, so that `make` on a macOS
# workstation produces something that runs there.
GOOS ?= $(shell $(GO) env GOHOSTOS)
GOARCH ?= $(shell $(GO) env GOHOSTARCH)
export GOOS
export GOARCH

# Platforms crawlc is expected to compile for. Windows is included so that the
# stub keeps building: it is the file that documents why the port is blocked, and
# it is worth knowing if it rots.
PLATFORMS := linux darwin freebsd windows

# The shim is built without cgo so that it keeps running when the host it was
# started on is upgraded underneath it. containerd builds its own shims the same
# way, and a shim that fails to exec is a different bug than the ones crawlc is
# meant to provoke.
export CGO_ENABLED := 0

##@ Local

.PHONY: all
all: build ## Build for the host (default)

.PHONY: build
build: bin/$(BINARY) ## Build for the host

bin/$(BINARY): $(shell find cmd internal pkg -name '*.go' 2>/dev/null) go.mod go.sum
	$(GO) build -o $@ ./cmd/containerd-shim-crawlc-v1

# Catches a change that only compiles on the platform it was written on.
.PHONY: build-all
build-all: ## Compile-check every platform
	@for os in $(PLATFORMS); do \
		printf '%-9s ' $$os; \
		GOOS=$$os $(GO) build -o /dev/null ./cmd/containerd-shim-crawlc-v1 || exit 1; \
		echo ok; \
	done

.PHONY: install
install: build ## Install to $(PREFIX)/bin
	install -D -m 0755 bin/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)

.PHONY: uninstall
uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)

.PHONY: test
test: ## Run unit tests
	$(GO) test ./...

# The integration suite starts containerd daemons, runs containers and leaves
# wedged shims behind, so it is opt-in: the target above compiles it and skips
# it. Needs root, a containerd and an OCI runtime on PATH, and the shim it is
# testing, which is why it depends on build.
#
# Point it at a particular containerd with CRAWLC_TEST_CONTAINERD and
# CRAWLC_TEST_CTR; see integration/main_test.go for the rest of the knobs.
INTEGRATION_FLAGS ?= -v -count=1 -timeout 20m

.PHONY: integration
integration: build ## Run integration tests against a real containerd (needs root)
	CRAWLC_TEST_INTEGRATION=1 $(GO) test $(INTEGRATION_FLAGS) ./integration/...

# gofmt walks the tree literally, so it has to be told about vendor; go vet,
# go test and golangci-lint all skip it on their own because ./... excludes
# vendored packages in module mode.
GOFILES = $(shell find . -path ./vendor -prune -o -name '*.go' -print)

.PHONY: verify
verify: ## gofmt and vet on every platform
	@fmtd="$$(gofmt -l $(GOFILES))"; \
	test -z "$$fmtd" || { echo 'gofmt:'; echo "$$fmtd"; exit 1; }
	@for os in $(PLATFORMS); do \
		printf 'vet %-9s ' $$os; \
		GOOS=$$os $(GO) vet ./... || exit 1; \
		echo ok; \
	done

##@ Containerised

# Every docker target is a thin wrapper over a bake target, so the Makefile and
# the release workflow run the identical build. VERSION and REVISION are exported
# here rather than passed per-recipe, because bake reads them from the
# environment and a missing one silently stamps the binary "dev".
VERSION ?= dev
REVISION ?= $(shell git rev-parse --short HEAD 2>/dev/null)
BAKE ?= docker buildx bake
export VERSION
export REVISION

# Wrapping bake in a pattern rule keeps one recipe for all of them; `make
# docker-lint` runs `bake lint`, and a new bake target needs no Makefile change.
.PHONY: docker-%
docker-%:
	$(BAKE) $*

.PHONY: cross
cross: docker-binaries ## Cross-compile every platform into ./bin

.PHONY: docker-verify
docker-verify: ## gofmt, vet and unit tests, in the image
	$(BAKE) verify

.PHONY: docker-lint
docker-lint: ## golangci-lint on every platform, in the image
	$(BAKE) lint

.PHONY: docker-validate
docker-validate: ## everything a change has to pass, in the image
	$(BAKE) validate

# Kept as aliases: these were the names before the docker- prefix existed.
.PHONY: cross-verify
cross-verify: docker-verify

# Rehearse a release locally. VERSION defaults to "dev" the same way the
# workflow's manual trigger does.
.PHONY: package
package: cross ## Rehearse a release into ./dist
	VERSION=$(VERSION) ./script/package.sh

##@ Other

.PHONY: clean
clean: ## Remove build and release output
	rm -rf bin dist

.PHONY: help
help: ## List the targets worth knowing about
	@awk 'BEGIN {FS = ":.*##"} \
		/^##@/ { printf "\n%s\n", substr($$0, 5) } \
		/^[a-zA-Z_-]+:.*?##/ { printf "  %-16s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
