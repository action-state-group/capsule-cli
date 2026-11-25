.PHONY: build install test check check-full

# capsulectl. The first-party plugins build in their own repositories
# (capsule-cli#326, docs/PLUGINS.md).
build:
	go build -o capsulectl ./cmd/capsulectl

install:
	go install ./cmd/capsulectl

test:
	bash scripts/test.sh

# Before every push: the quality checks and both test passes, the race pass
# sampled (scripts/check.sh). check-full runs every race case and the report
# page in Chrome, as the nightly does.
check:
	bash scripts/check.sh

check-full:
	bash scripts/check.sh --full
