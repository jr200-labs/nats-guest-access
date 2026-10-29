.PHONY: build test lint

build:
	go build ./...

test:
	go test -race ./...

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"
