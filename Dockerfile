# syntax=docker/dockerfile:1

# crawlc is pure Go with cgo off, so every target cross-compiles from whatever
# host runs the build. The builder stage is pinned to BUILDPLATFORM for that
# reason: emulating the target would be slower and buys nothing, and for the
# darwin and windows targets there is no image to emulate in the first place.

ARG GO_VERSION=1.26
ARG GOLANGCI_VERSION=v2.12
ARG BINARY=containerd-shim-crawlc-v1

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS base
WORKDIR /src
ENV CGO_ENABLED=0
# Dependencies come from the vendor directory, so there is nothing to download
# and no module cache to warm. -mod=vendor is set rather than left to Go's
# auto-detection so that an incomplete or missing vendor tree fails here instead
# of quietly reaching for the network and building something else.
ENV GOFLAGS=-mod=vendor

FROM base AS src
COPY . .

FROM src AS build
ARG TARGETOS
ARG TARGETARCH
ARG BINARY
ARG VERSION
ARG REVISION
# crawlc embeds containerd's shim framework, so `-v` reports whatever is in
# containerd's version package. Stamping it means a released binary identifies
# itself as crawlc rather than as the containerd it was compiled against.
ARG VERSION_PKG=github.com/containerd/containerd/v2/version
# --network=none turns "we vendor, so this should not need the network" into
# something the build enforces: a dependency that escapes vendor/ fails here
# rather than silently succeeding on a machine that happens to be online.
RUN --network=none --mount=type=cache,target=/root/.cache/go-build \
    ext=''; \
    # containerd looks for a .exe on Windows; see shimBinaryFormat in
    # pkg/shim/util_windows.go. The name has to match or the shim is never found.
    if [ "$TARGETOS" = 'windows' ]; then ext='.exe'; fi; \
    GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath \
        -ldflags "-s -w -X ${VERSION_PKG}.Version=${VERSION} -X ${VERSION_PKG}.Revision=${REVISION}" \
        -o "/out/${BINARY}${ext}" .

# Unit tests and vet run once on the build platform rather than per target. The
# code they cover is the platform-independent half, so running it six times would
# only repeat itself.
FROM src AS verify
RUN --network=none --mount=type=cache,target=/root/.cache/go-build \
    # gofmt walks the tree literally and would report vendored third-party code
    # that is not ours to reformat. vet, test and lint need no such exclusion:
    # ./... already skips vendored packages in module mode.
    fmtd="$(find . -path ./vendor -prune -o -name '*.go' -print | xargs gofmt -l)"; \
    test -z "$fmtd" || { echo 'gofmt:'; echo "$fmtd"; exit 1; }
RUN --network=none --mount=type=cache,target=/root/.cache/go-build \
    for os in linux darwin freebsd windows; do \
        printf 'vet %-9s ' "$os"; GOOS="$os" go vet ./... || exit 1; echo ok; \
    done
RUN --network=none --mount=type=cache,target=/root/.cache/go-build \
    go test ./...

# golangci-lint comes from its own published image rather than being installed
# with `go install`, so the version is pinned by digest resolution and a lint run
# never has to compile the linter first.
FROM golangci/golangci-lint:${GOLANGCI_VERSION}-alpine AS golangci

FROM src AS lint
COPY --from=golangci /usr/bin/golangci-lint /usr/bin/golangci-lint
# Linting is per-GOOS for the same reason vet is: most of crawlc sits behind a
# build tag, and a single-platform run would not read the files it is meant to
# check. The cache is keyed per platform so the runs do not evict each other.
RUN --network=none --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/.cache/golangci-lint \
    for os in linux darwin freebsd windows; do \
        printf 'lint %-9s ' "$os"; \
        GOOS="$os" GOGC=50 golangci-lint run --timeout 5m ./... || exit 1; \
        echo ok; \
    done

# Scratch keeps the export to a bare binary, and means the darwin and windows
# targets never need a base image for a platform that cannot run a container.
FROM scratch AS binary
COPY --from=build /out/ /
