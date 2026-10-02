.PHONY: build install test iife-check

build:
	go build -o capsulectl ./cmd/capsulectl

install:
	go install ./cmd/capsulectl

test:
	bash scripts/test.sh

# Rebuild the vendored evidence-graph IIFE from an agent-action-capsule
# checkout (AAC=path, at the commit internal/cli/assets/README.md names) and
# diff it against internal/cli/assets/evidence-graph.iife.js.
iife-check:
	bash scripts/iife-sync.sh "$(AAC)" --check
