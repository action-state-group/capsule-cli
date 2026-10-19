.PHONY: build install test iife-check

# capsulectl and its first-party plugins (capsule-cli#236); `make install`
# installs all three (docs/PLUGINS.md).
build:
	go build -o capsulectl ./cmd/capsulectl
	cd plugins && go build -o ../capsulectl-book ./cmd/capsulectl-book && go build -o ../capsulectl-deal ./cmd/capsulectl-deal

# Installs capsulectl, then both plugins onto ~/.local/lib/capsulectl/plugins
# through the capsulectl just installed (found where go install put it, never
# on PATH, so an older release core cannot refuse the dev-built plugins).
install: build
	go install ./cmd/capsulectl
	bin="$$(go env GOBIN)"; gopath="$$(go env GOPATH)"; core="$${bin:-$${gopath%%:*}/bin}/capsulectl"; \
	"$$core" plugin install ./capsulectl-book && "$$core" plugin install ./capsulectl-deal

test:
	bash scripts/test.sh

# Rebuild the vendored evidence-graph IIFE from an agent-action-capsule
# checkout (AAC=path, at the commit internal/cli/assets/README.md names) and
# diff it against internal/cli/assets/evidence-graph.iife.js.
iife-check:
	bash scripts/iife-sync.sh "$(AAC)" --check
