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
PKG=github.com/action-state-group/capsule-cli/cli
TARGETS=(linux/amd64 linux/arm64 darwin/arm64)

# The first-party plugins (capsule-cli#236) are two more binaries per
# target, built from the plugins module: capsulectl-book and capsulectl-deal.
# Their assets are named capsulectl-VERSION-plugin-NAME-OS-ARCH, so
# SHA256SUMS and the build attestation cover them like the core binary; an operator installs
# each as capsulectl-NAME on a plugin root (docs/PLUGINS.md).
PLUGINS=(book deal)

mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
ldflags="-s -w -buildid= -X $PKG.CLIVersion=$VERSION -X $PKG.CLICommit=$COMMIT"
for target in "${TARGETS[@]}"; do
  os=${target%/*} arch=${target#*/}
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags "$ldflags" -o "$OUT/capsulectl-$VERSION-$os-$arch" ./cmd/capsulectl
  for plugin in "${PLUGINS[@]}"; do
    (cd plugins && GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -buildvcs=false \
      -ldflags "$ldflags" -o "$OUT/capsulectl-$VERSION-plugin-$plugin-$os-$arch" "./cmd/capsulectl-$plugin")
  done
done

cd "$OUT"
if command -v sha256sum >/dev/null; then
  sha256sum capsulectl-"$VERSION"-* > SHA256SUMS
else
  shasum -a 256 capsulectl-"$VERSION"-* > SHA256SUMS
fi
cat SHA256SUMS
