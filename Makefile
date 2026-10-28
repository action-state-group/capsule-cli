.PHONY: build install test

# capsulectl. The first-party plugins build in their own repositories
# (capsule-cli#326, docs/PLUGINS.md).
build:
	go build -o capsulectl ./cmd/capsulectl

install:
	go install ./cmd/capsulectl

test:
	bash scripts/test.sh
