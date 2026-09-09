.PHONY: build install test

build:
	go build -o capsule ./cmd/capsule

install:
	go install ./cmd/capsule

test:
	bash scripts/test.sh
