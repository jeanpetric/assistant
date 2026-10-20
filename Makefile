.PHONY: build test run reset

build:
	go build -o assistant ./cmd/assistant

test:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race -count=1 ./...

run: build
	./assistant

reset: build
	scripts/reset.sh
	./assistant
