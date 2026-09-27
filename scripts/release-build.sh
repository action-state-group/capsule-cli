#!/usr/bin/env bash
# release-build.sh VERSION COMMIT OUTDIR -- the one build command for release
# binaries, used by .github/workflows/release.yml and for local rebuilds.
#
# Reproducible: CGO off (SQLite is modernc.org/sqlite, pure Go), -trimpath,
# no VCS stamping, empty build id, and only VERSION/COMMIT injected. The same
# Go toolchain (go.mod's `go` line) and the same inputs give byte-identical
# binaries, so anyone can rebuild a tag and compare against SHA256SUMS.
set -euo pipefail

VERSION="${1:?version, e.g. v0.1.0}"
COMMIT="${2:?full commit sha}"
OUT="${3:?output directory}"
PKG=github.com/action-state-group/capsule-cli/internal/cli
TARGETS=(linux/amd64 linux/arm64 darwin/arm64)

mkdir -p "$OUT"
for target in "${TARGETS[@]}"; do
  os=${target%/*} arch=${target#*/}
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X $PKG.cliVersion=$VERSION -X $PKG.cliCommit=$COMMIT" \
    -o "$OUT/capsulectl-$VERSION-$os-$arch" ./cmd/capsulectl
done

cd "$OUT"
if command -v sha256sum >/dev/null; then
  sha256sum capsulectl-"$VERSION"-* > SHA256SUMS
else
  shasum -a 256 capsulectl-"$VERSION"-* > SHA256SUMS
fi
cat SHA256SUMS
