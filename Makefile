.PHONY: build test run start stop reset

build:
	go build -o assistant ./cmd/assistant

test:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go test -race -count=1 ./...

run: build
	./assistant

start: build
	scripts/start.sh

stop:
	scripts/stop.sh

reset: build
	scripts/reset.sh
	scripts/start.sh
