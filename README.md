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
- **Agent sessions per task**: start as many as you want, with the model of your choice, on
  autopilot (`bypassPermissions`) or a stricter mode. Each session is an interactive `claude`
  running in a detached tmux session (`assistant-<id>`): replies are typed into it, and you can
  attach (`tmux attach -t assistant-<id>`, button on the session) to watch or take over. If the
  tmux session is gone, the next reply starts a new one with `claude --resume`.
- **Sessions overview**: every session across all tasks with status and latest message.
- **Transition rules**: when a task moves from lane A (or any lane) to lane B, start an agent
  session with a prompt template — e.g. run a skill like `/p-dev:code` when a task enters
  *In progress*.
- **Recurring schedules** per task (cron syntax): start an agent session or create a reminder.
- **Notifications** for reminders and failed automations.

## Run

```sh
make build
./assistant                      # http://127.0.0.1:6767
```

Settings live in `~/.assistant/config.json` (created with defaults on first run):

```json
{
  "port": 6767,
  "db": "~/.assistant/assistant.db",
  "claude": "claude",
  "claude_args": "",
  "workdir": "$HOME",
  "mode": "bypassPermissions",
  "model": "",
  "models": "opus,sonnet,haiku",
  "runner": "tmux",
  "tmux_socket": ""
}
```

Every key also has a flag that overrides the file for one run: `-port`, `-db`, `-claude`,
`-claude-args`, `-workdir`, `-mode`, `-model`, `-models`, `-runner`, `-tmux-socket`, plus `-config` to point at another file. The server always
binds to 127.0.0.1. Avoid ports 6665–6669: browsers refuse them.

`model` is the default model (empty: the claude CLI default); `models` are the choices offered when
starting a session. `runner` is `tmux` (needs tmux) or `print`, which runs a headless `claude -p`
per turn instead. The tmux runner learns about turns through `SessionStart`/`Stop` hooks it passes
with `--settings`; their events are kept in `agents/` next to the database. A workdir claude has
never seen shows a folder trust prompt, which the runner accepts.

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
