#!/usr/bin/env bash
#
# Package the cross-compiled binaries under bin/ into release archives.
#
# Lives in a script rather than inline in the workflow so that a release can be
# rehearsed locally, which is the only way to find out what the artifacts
# actually look like before a tag makes them permanent.
#
#   VERSION=v0.1.0 ./script/package.sh

set -euo pipefail

VERSION="${VERSION:-dev}"
BINARY="${BINARY:-containerd-shim-crawlc-v1}"
BINDIR="${BINDIR:-bin}"
DISTDIR="${DISTDIR:-dist}"

# Fixed timestamp for every archive entry, so that rebuilding a commit produces
# byte-identical artifacts and a checksum can be compared against a rebuild.
SOURCE_DATE="${SOURCE_DATE:-2020-01-01T00:00:00Z}"

# Archive names carry the bare version, so v0.1.0 and 0.1.0 produce the same file
# whichever form the caller passes.
version="${VERSION#v}"

if [ ! -d "$BINDIR" ]; then
    echo "$0: $BINDIR does not exist; run 'docker buildx bake' first" >&2
    exit 1
fi

# zip is not installed everywhere a release might be rehearsed, and discovering
# that after building six archives is worse than saying so now. Python's zipfile
# is an adequate stand-in and is far more likely to be present.
make_zip() { # make_zip <dest.zip> <dir>
    if command -v zip >/dev/null 2>&1; then
        (cd "$2" && zip -q -X -r "$1" .)
    elif command -v python3 >/dev/null 2>&1; then
        (cd "$2" && python3 -m zipfile -c "$1" ./*)
    else
        echo "$0: need either zip or python3 to build Windows archives" >&2
        exit 1
    fi
}

rm -rf "$DISTDIR"
mkdir -p "$DISTDIR"

staged=0
for dir in "$BINDIR"/*/; do
    [ -d "$dir" ] || continue
    platform="$(basename "$dir")" # linux_amd64, darwin_arm64, ...
    os="${platform%%_*}"

    exe=""
    if [ "$os" = "windows" ]; then
        exe=".exe"
    fi

    src="${dir}${BINARY}${exe}"
    if [ ! -f "$src" ]; then
        echo "$0: expected $src, not found" >&2
        exit 1
    fi

    stage="$(mktemp -d)"
    trap 'rm -rf "$stage"' EXIT
    install -m 0755 "$src" "$stage/${BINARY}${exe}"
    install -m 0644 LICENSE README.md "$stage/"

    # zip records each entry's mtime and has no equivalent of tar's --mtime, so
    # the timestamps are pinned on disk instead. Without this the zips differ on
    # every run even when their contents are identical.
    find "$stage" -exec touch -d "$SOURCE_DATE" {} +

    name="crawlc_${version}_${platform}"

    # The SBOM ships beside the archive rather than inside it, so that it can be
    # fetched and diffed without downloading a binary. Buildkit writes it per
    # platform; a build run without attestations simply has none.
    if [ -f "${dir}sbom.spdx.json" ]; then
        cp "${dir}sbom.spdx.json" "${DISTDIR}/${name}.sbom.spdx.json"
    fi
    if [ "$os" = "windows" ]; then
        # zip for Windows, where tar is not a given outside recent builds.
        make_zip "$(pwd)/${DISTDIR}/${name}.zip" "$stage"
    else
        # Pinned ownership, sorted entries and a fixed mtime, so that rebuilding
        # the same commit produces a byte-identical archive.
        tar --sort=name --owner=0 --group=0 --numeric-owner \
            --mtime="$SOURCE_DATE" \
            -czf "${DISTDIR}/${name}.tar.gz" -C "$stage" .
    fi

    rm -rf "$stage"
    trap - EXIT
    staged=$((staged + 1))
done

if [ "$staged" -eq 0 ]; then
    echo "$0: no platform directories found under $BINDIR" >&2
    exit 1
fi

# Checksums are relative to DISTDIR so that `sha256sum -c` works from inside the
# directory a user unpacks into, rather than only from the repository root.
#
# The list is found rather than globbed: any of these patterns can legitimately
# match nothing — a linux-only build has no .zip, and a build without
# attestations has no SBOM — and an unmatched glob reaches sha256sum as a literal
# filename, which fails the whole script under `set -e` with a half-written
# SHA256SUMS behind it. Sorting keeps the file stable across runs.
(cd "$DISTDIR" && find . -maxdepth 1 -type f \
    \( -name '*.tar.gz' -o -name '*.zip' -o -name '*.sbom.spdx.json' \) -print0 \
    | sort -z | xargs -0 sha256sum | sed 's| \./| |' > SHA256SUMS)

echo "packaged $staged archive(s) into $DISTDIR:"
ls -1 "$DISTDIR"
