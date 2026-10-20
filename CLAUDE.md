# assistant — agent instructions

Personal workflow app: Go backend + vanilla JS UI embedded in one binary, SQLite storage.

## Rules

- **Minimal dependencies.** Only `modernc.org/sqlite` (pure Go, keeps the binary cgo-free).
  Ask before adding any Go module or any frontend library. No JS build step.
- **Tests decide.** Every change comes with tests; `make test` (gofmt, vet, `go test -race`) must
  be green before anything lands on `main`. CI runs the same on every push.
- Commit and push freely; `main` is not protected. Use a branch for larger changes and merge
  once green.

## Layout

- `cmd/assistant` — config + flags, wiring, HTTP server lifecycle.
- `internal/config` — `~/.assistant/config.json`; new settings get a JSON key and a flag.
- `internal/store` — all SQL. Schema changes go in a **new** entry appended to `migrations`
  in `migrate.go`; never edit an applied migration. The DB has a single connection — don't run
  a query while iterating another query's rows.
- `internal/agent` — runs `claude -p --output-format stream-json`; `ParseLine` maps stream
  events to transcript messages. Tests use a fake CLI script, never the real `claude`.
- `internal/app` — behaviour: sessions (start / resume / cancel), transition rules on lane
  moves, schedules, prompt templates.
- `internal/cron` — 5-field cron parser.
- `internal/server` — JSON API (`handlers.go`), request guard (`server.go`), embedded UI in
  `web/` (`app.js` is plain DOM code with a tiny `h()` helper — never use `innerHTML` with data).
