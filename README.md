# assistant

A personal, local-first workflow board for an engineering manager who also writes code.
Tasks live on a kanban board; each task can run any number of Claude Code agent
sessions, and the **whole** transcript of every session is shown on the task page.

One Go binary, one SQLite file, no JavaScript build step.

## Features

- **Kanban board** with user-defined lanes and drag & drop; a **list view** with filtering.
- **Flexible tasks**: title, description, kind (task / coding / research / discussion / reminder),
  working directory, and **custom fields** (Productive task, pull request, Slack thread out of
  the box — add more in Settings).
- **Agent sessions per task**: start as many as you want, on autopilot
  (`bypassPermissions`) or a stricter mode. Reply to continue a session (uses `claude --resume`),
  stop it, or copy the resume command to continue in a terminal.
- **Sessions overview**: every session across all tasks with status and latest message.
- **Transition rules**: when a task moves from lane A (or any lane) to lane B, start an agent
  session with a prompt template — e.g. run a skill like `/p-dev:code` when a task enters
  *In progress*.
- **Recurring schedules** per task (cron syntax): start an agent session or create a reminder.
- **Notifications** for reminders and failed automations.

## Run

```sh
make build
./assistant                      # http://127.0.0.1:7777
```

Flags:

| flag | default | |
|---|---|---|
| `-addr` | `127.0.0.1:7777` | must be a loopback address |
| `-db` | `~/.assistant/assistant.db` | SQLite file |
| `-claude` | `claude` | CLI binary |
| `-claude-args` | | extra args for every run, e.g. `"--model opus"` |
| `-workdir` | `$HOME` | default agent working directory |
| `-mode` | `bypassPermissions` | default permission mode |

Agents run with your user's permissions. The server only binds to localhost and rejects
non-JSON writes, foreign `Origin`s and foreign `Host` headers so that web pages can't drive it.

## Prompt templates

Rules and schedules use Go `text/template`:

- `{{.Context}}` — title, description and links as a plain-text brief
- `{{.Task.Title}}`, `{{.Task.Description}}`, `{{.Task.ID}}`
- `{{.Fields.pull_request}}`, `{{.Fields.productive}}`, … (any custom field key)

Example rule prompt: `/p-dev:review {{.Fields.pull_request}}`

## Development

```sh
make test   # gofmt + vet + race tests; CI runs the same on every push
```

`main` is merged into only when the suite is green. See `CLAUDE.md` for conventions.
