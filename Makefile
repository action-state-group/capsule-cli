.PHONY: build install test check check-full

# capsulectl. The first-party plugins build in their own repositories
# (capsule-cli#326, docs/PLUGINS.md).
build:
	go build -o capsulectl ./cmd/capsulectl

install:
	go install ./cmd/capsulectl

test:
	bash scripts/test.sh

# Before every push: the quality checks and both test passes (scripts/check.sh).
# check-full is check plus the report page in Chrome.
check:
	bash scripts/check.sh

check-full:
	bash scripts/check.sh --full
