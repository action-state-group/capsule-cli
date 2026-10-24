.PHONY: build install test iife-check

# capsulectl. The first-party plugins build in their own repositories
# (capsule-cli#326, docs/PLUGINS.md).
build:
	go build -o capsulectl ./cmd/capsulectl

install:
	go install ./cmd/capsulectl

test:
	bash scripts/test.sh

# Rebuild the vendored evidence-graph IIFE from a capsule-viewer checkout
# (CV=path) and an agent-action-capsule checkout (AAC=path), each at the
# commit cli/assets/README.md names, and diff it against
# cli/assets/evidence-graph.iife.js.
iife-check:
	bash scripts/iife-sync.sh "$(CV)" "$(AAC)" --check
