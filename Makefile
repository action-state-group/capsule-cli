.PHONY: build install test

build:
	go build -o capsulectl ./cmd/capsulectl

install:
	go install ./cmd/capsulectl

test:
	bash scripts/test.sh
