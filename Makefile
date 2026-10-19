.PHONY: build install test iife-check

# capsulectl and its first-party plugins (capsule-cli#236); install the
# plugins as capsulectl-book and capsulectl-deal on a plugin root
# (docs/PLUGINS.md).
build:
	go build -o capsulectl ./cmd/capsulectl
	cd plugins && go build -o ../capsulectl-book ./cmd/capsulectl-book && go build -o ../capsulectl-deal ./cmd/capsulectl-deal

install:
	go install ./cmd/capsulectl

test:
	bash scripts/test.sh

# Rebuild the vendored evidence-graph IIFE from an agent-action-capsule
# checkout (AAC=path, at the commit internal/cli/assets/README.md names) and
# diff it against internal/cli/assets/evidence-graph.iife.js.
iife-check:
	bash scripts/iife-sync.sh "$(AAC)" --check
