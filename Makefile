.PHONY: build install

build:
	go build -o capsule ./cmd/capsule

install:
	go install ./cmd/capsule
