.PHONY: build install test check

# capsulectl. The first-party plugins build in their own repositories
# (capsule-cli#326, docs/PLUGINS.md).
build:
	go build -o capsulectl ./cmd/capsulectl

install:
	go install ./cmd/capsulectl

test:
	bash scripts/test.sh

# Before every push: every pull request job, both test passes included
# (scripts/check.sh).
check:
	bash scripts/check.sh
