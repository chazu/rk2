.PHONY: build install test check acceptance
build:
	go build -o bin/rk ./cmd/rk

install:
	go install ./cmd/rk

test:
	go test -race ./...

check:
	go vet ./...
	go test -race ./...
	go build -o bin/rk ./cmd/rk
	python3 tests/acceptance.py

acceptance: build
	python3 tests/acceptance.py
