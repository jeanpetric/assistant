.PHONY: build test run

build:
	go build -o assistant ./cmd/assistant

test:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race -count=1 ./...

run: build
	./assistant
