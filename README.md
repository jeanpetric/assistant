# assistant

A personal, local-first workflow board for an engineering manager who also writes code.
Tasks live on a kanban board; each task can run any number of Claude Code agent
sessions, and the **whole** transcript of every session is shown on the task page.

One Go binary, one SQLite file, no JavaScript build step.

## Features

- **Kanban board** with user-defined lanes and drag & drop; a **list view** with filtering.
  Clicking a card opens the task in a dialog over the board; *⤢ Full page* (or ⌘/Ctrl-click on
  the card) opens its full task page.
- **Projects**: separate boards (e.g. private, one per area of work) sharing the same lanes. Pick
  one at the top; the board, list and archive show only its tasks. Create, rename and reorder
  them in *Settings → Projects*, where each can also get a default working directory for its
  tasks' agents; a task moves to another project from its page.
- **Flexible tasks**: title, description, kind (task / coding / research / discussion / reminder),
  working directory, and **custom fields** (Productive task, pull request, Slack thread out of
  the box — add more in Settings).
- **Agent sessions per task**: start as many as you want, on autopilot
  (`bypassPermissions`) or a stricter mode. Reply to continue a session (uses `claude --resume`),
  stop it, or copy the resume command to continue in a terminal.
- **Sessions overview**: every session across all tasks with status and latest message.
- **Working directories**: list the directories agents may run in under *Settings → Working
  directories*; the task, new session, standalone session and rule forms pick from that list
  (empty means the task's directory, else its project's, else the app default `workdir`). Upgrading seeds the list
  with every directory already used.
- **Transition rules**: when a task moves from lane A (or any lane) to lane B, start an agent
  session with a prompt template — e.g. run a skill like `/p-dev:code` when a task enters
  *In progress*.
- **Move when done**: a session can move its task to another lane (e.g. *Review*) once the agent
  finishes a turn successfully. Pick it when starting a session or change it on the session;
  rules and schedules set it for the sessions they start (new rules default to *Review*). A
  failed turn, or a task you moved by hand meanwhile, stays put.
- **Archive**: tasks that sit in a chosen lane (*Done* by default) for a set time (a week by
  default) are archived automatically; both are in *Settings → Archive* (pick *Off* to stop it).
  A task can also be archived by hand from its page. Archived tasks leave the board and are
  listed under *Archive* in the top menu; *Restore* (or moving it to a lane) puts one back.
- **Due times & quick reminders**: any task can have a due time; when it's reached you get a
  notification. *⏰ Reminder* (or press `r`) creates a reminder task in two keystrokes, with
  presets like *15 min* or *Tomorrow 9:00*.
- **Recurring schedules** per task (cron syntax): start an agent session or create a reminder.
- **Notifications** for reminders, due tasks and failed automations — also as desktop
  notifications while the board is open in a browser tab (allow them when asked).
- **Keyboard shortcuts** outside text fields: `n` new task, `r` reminder, `s` session, `/` filter,
  `[` / `]` previous / next project, `g` then `b` `l` `s` `a` `,` to jump to Board, List,
  Sessions, Archive or Settings, `i` notifications. Each shortcut shows as a greyed key next to
  its button or link; press `?` (or the `?` button) for the full list.

## Run

```sh
make start    # build and run in the background: http://127.0.0.1:6767, log in ~/.assistant/assistant.log
make stop
make restart  # rebuild and restart, keeps all data
make reset    # DELETES all data (asks first; backup in ~/.assistant/backups/), start fresh
make run      # run in the foreground instead
```

Settings live in `~/.assistant/config.json` (created with defaults on first run):

```json
{
  "port": 6767,
  "db": "~/.assistant/assistant.db",
  "claude": "claude",
  "claude_args": "",
  "workdir": "$HOME",
  "mode": "bypassPermissions"
}
```

Every key also has a flag that overrides the file for one run: `-port`, `-db`, `-claude`,
`-claude-args`, `-workdir`, `-mode`, plus `-config` to point at another file. The server always
binds to 127.0.0.1. Avoid ports 6665–6669: browsers refuse them.

Agents run with your user's permissions. The server only binds to localhost and rejects
non-JSON writes, foreign `Origin`s and foreign `Host` headers so that web pages can't drive it.

## MCP server

The app is also an MCP server, so any Claude Code session (including the agents it starts)
can read and change the board: `http://127.0.0.1:6767/mcp` (Streamable HTTP). Register it once
for all your sessions:

```sh
claude mcp add --scope user --transport http assistant http://127.0.0.1:6767/mcp
```

Tools: `get_board`, `get_task`, `create_task`, `update_task`, `move_task`, `archive_task`,
`unarchive_task`, `delete_task`, `list_archived_tasks`, `create_lane`, `update_lane`,
`delete_lane`, `create_project`, `update_project`, `delete_project`, `start_session`, `list_sessions`, `get_session`, `send_message`. Each one goes
through the JSON API, so validation and transition rules work as in the UI. The app must be
running (`make start`).

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
