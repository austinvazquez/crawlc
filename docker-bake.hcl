// Cross-compilation matrix for crawlc.
//
//   docker buildx bake            # binaries for every platform into ./bin
//   docker buildx bake verify     # gofmt, vet and unit tests
//   docker buildx bake linux      # one platform family
//
// Binaries land in bin/<os>_<arch>/, which is buildkit's layout whenever a local
// export covers more than one platform.

variable "GO_VERSION" {
  default = "1.26"
}

variable "GOLANGCI_VERSION" {
  default = "v2.12"
}

variable "BINARY" {
  default = "containerd-shim-crawlc-v1"
}

variable "DESTDIR" {
  default = "./bin"
}

// Stamped into the binary so that `containerd-shim-crawlc-v1 -v` reports crawlc
// rather than the containerd release it was built against. The release workflow
// passes the tag and commit; a local build stays honest by saying "dev".
variable "VERSION" {
  default = "dev"
}

variable "REVISION" {
  default = ""
}

group "default" {
  targets = ["binaries"]
}

target "_common" {
  dockerfile = "Dockerfile"
  args = {
    GO_VERSION       = GO_VERSION
    GOLANGCI_VERSION = GOLANGCI_VERSION
    BINARY           = BINARY
    VERSION          = VERSION
    REVISION         = REVISION
  }
}

// Windows on arm64 is included because it costs nothing to compile and catches
// build-tag mistakes, not because crawlc runs there: containerd's pkg/shim
// cannot serve a shim on Windows, so both windows targets build the stub.
target "binaries" {
  inherits = ["_common"]
  target   = "binary"
  output   = ["type=local,dest=${DESTDIR}"]

  // With a local export these land as provenance.json and sbom.spdx.json inside
  // each platform directory rather than being attached to a manifest, so they
  // are unsigned files: transparency, not integrity. Checksums and the signed
  // provenance in the release workflow are what cover integrity.
  //
  // The SBOM is worth having regardless. crawlc links containerd statically, so
  // the module list in the binary is the only record of which containerd a given
  // artifact carries when a CVE lands.
  attest = [
    "type=provenance,mode=max",
    "type=sbom",
  ]
  platforms = [
    "linux/amd64",
    "linux/arm64",
    "darwin/amd64",
    "darwin/arm64",
    "windows/amd64",
    "windows/arm64",
  ]
}

target "linux" {
  inherits  = ["binaries"]
  platforms = ["linux/amd64", "linux/arm64"]
}

target "darwin" {
  inherits  = ["binaries"]
  platforms = ["darwin/amd64", "darwin/arm64"]
}

target "windows" {
  inherits  = ["binaries"]
  platforms = ["windows/amd64", "windows/arm64"]
}

// Neither of these produces artifacts; they are run for their exit status.
target "verify" {
  inherits = ["_common"]
  target   = "verify"
  output   = ["type=cacheonly"]
}

target "lint" {
  inherits = ["_common"]
  target   = "lint"
  output   = ["type=cacheonly"]
}

// Everything a change has to pass. The two run concurrently, since neither
// depends on the other's result.
group "validate" {
  targets = ["verify", "lint"]
}
