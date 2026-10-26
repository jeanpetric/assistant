.PHONY: build test run start stop restart reset backup

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

# Rebuild and restart; the database is kept.
restart: build
	scripts/stop.sh
	scripts/start.sh

# Consistent copy of the database into ~/.assistant/backups, safe while the app runs.
backup:
	scripts/backup.sh

# Wipes all data (after a backup); asks for confirmation first.
reset: build
	scripts/reset.sh
	scripts/start.sh
