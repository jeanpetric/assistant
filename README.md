# assistant

A personal, local-first workflow board for an engineering manager who also writes code.
Tasks live on a kanban board; each task can run any number of Claude Code or Gemini CLI
agent sessions, and the **whole** transcript of every session is shown on the task page.

One Go binary, one SQLite file, no JavaScript build step.

## Features

### Board and tasks

- **Kanban board** with user-defined lanes and drag & drop; a **list view** with filtering.
  Clicking a card opens the task in a dialog over the board; *⤢ Full page* (or ⌘/Ctrl-click on
  the card) opens its full task page. *Copy link* (`⌥C`) copies `http://…/#/task/<id>`.
- **Projects**: separate boards (e.g. private, one per area of work) sharing the same lanes. Pick
  one at the top or with `⌥K`; the board, list and archive show only its tasks. Create, rename
  and reorder them in *Settings → Projects*, where each can also get a default working directory
  for its agents; a task moves to another project from its page.
- **Flexible tasks**: title, description, kind (task / coding / research / discussion / reminder
  out of the box — add and delete kinds in *Settings → Task kinds*), working directory, and **custom fields** (Productive task, pull request, Slack thread out of
  the box — add more in Settings). In the *New task* dialog `⌘↵` creates the task from any field.
- **Builds on**: a task can build on earlier tasks (search by words or `#id`). Its agents then get
  a summary of each source task's sessions after the task brief; *+ Follow-up task* creates one,
  and cards show `↳ #id`. Summaries are written by a fork of the session's agent in the
  background, or on request with *Summarize*.
- **Archive**: tasks that sit in a chosen lane (*Done* by default) for a set time (a week by
  default) are archived automatically; both are in *Settings → Archive* (pick *Off* to stop it).
  A task can also be archived by hand from its page. Archived tasks are listed under *Archive*;
  *Restore* (or moving it to a lane) puts one back.
- **Due times & quick reminders**: any task can have a due time; when it's reached you get a
  notification. *⏰ Reminder* (`⌥R`) creates a reminder task in two keystrokes, with presets
  like *15 min* or *Tomorrow 9:00*.

### Agent sessions

- **Sessions per task**: start as many as you want, on autopilot (`bypassPermissions`) or a
  stricter mode, with a model of your choice. The agent gets the task brief — title,
  description, fields and links; linked Productive, Slack and other pages are read first.
  Reply to continue a session (`claude --resume`), stop it, resume one interrupted by a restart,
  or copy the resume command to continue in a terminal. Messages render as Markdown, tool calls
  fold into one collapsed group per run, and a *Working…* line shows the turn's elapsed time.
- **Gemini sessions**: pick *Gemini CLI* as a session's agent to run it on `gemini` instead of
  `claude`; modes map to its approval modes (autopilot is `yolo`). Gemini cannot fork a
  conversation, so its sessions' summaries and memory updates are written from the transcript
  by `gemini` (flash); skill models apply to Claude sessions only. With `"agent": "gemini"` in
  the config, new sessions, rules and schedules default to Gemini and the app needs no `claude`.
- **Standalone sessions** (`⌥P`): an agent without a task, run in the current project's
  directory; it starts in the background and shows on the *Sessions* page.
- **Agent status** on every card and task: running, ✋ *needs you* (the turn ended with a
  question, a permission denial or a plan to approve — also a notification), turn finished,
  done, failed or cancelled, until you've seen it. *Dismiss* on the session clears it.
- **Hand off**: when a session's context fills up, its agent writes a self-contained brief and a
  fresh session continues from it, with the same task, mode, model and directory.
- **Usage limits**: Claude's 5-hour and weekly usage left is shown under a running session.
- **Sessions overview**: every session across all tasks with status and latest message.
- **Working directories**: list the directories agents may run in under *Settings → Working
  directories*; task and rule forms pick from that list (empty means the task's directory, else
  its project's, else the app default `workdir`).

### Automation

- **Transition rules**: when a task of a project (or any project) moves from lane A (or any
  lane) to lane B, start an agent session with a prompt template — e.g. run `/p-dev:code` when a
  task enters *In progress* — or run a skill in the task's latest session, e.g. a self-review when
  it enters *Review* (in a new session if the task has none or its latest is working). A rule's done lane follows the session it starts, or its skill's turn.
- **Move when done**: a session can move its task to another lane (e.g. *Review*) once the agent
  finishes a turn successfully. Pick it when starting a session or change it on the session;
  rules and schedules set it for the sessions they start. A failed turn, or a task you moved by
  hand meanwhile, stays put.
- **Skills**: prompt templates an agent applies to its own session's work, as the next turn
  (*Settings → Skills*). Each skill has automations — task kind, project and lane conditions —
  and runs after a successful turn on a matching task, before the task moves to its done lane;
  run one by hand with *Run skill…*. A skill can use its own model for its turn and keep the
  reply as an **artifact** shown on the task. Ships with a *Research summary* skill (off).
- **Recurring schedules** per task (cron syntax): start an agent session or create a reminder.
- **Notifications** for reminders, due tasks, agents that need you and failed automations — also
  as desktop notifications while the board is open in a browser tab. Clicking one opens the
  task or session.

### Memory

Lasting knowledge on a topic, kept across tasks and projects, one Markdown document each, under
*Memory* (`⌥O`), most recently used first and searchable by words. While you type a new task the
*New task* dialog suggests memories on its topic; one click links them (a follow-up also gets
those of the task it builds on). *+ Task on this topic* on a memory starts a linked task, and
*+ New memory* on a task creates one, optionally written from its sessions so far. Agents of
linked tasks read them with the task context, and after every successful turn a fork of the
agent rewrites each one with what the session found out — or leaves it as it is. Every change is
a revision with a diff you can restore; only a failed update notifies. Edit a
memory by hand, update it from a session with *Update memory*, or from any Claude Code session
via the MCP tools `search_memories` and `remember`.

### Keyboard shortcuts

With ⌥ (Option/Alt), working in text fields too: `⌥T` new task, `⌥R` reminder, `⌥P` session,
`⌥C` copy task link, `⌥M` notifications, `⌥K` project picker, `⌥[` / `⌥]` previous / next
project, and `⌥B` `⌥L` `⌥S` `⌥O` `⌥A` `⌥,` to jump to Board, List, Sessions, Memory, Archive or
Settings. They go by the physical key, so ⌥ never types a special character for them. Outside
text fields `/` focuses the filter and `?` lists the shortcuts; `Esc` leaves a text field or
closes a dialog. Each shortcut shows as a greyed key next to its button or link.

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
  "gemini": "gemini",
  "gemini_args": "",
  "workdir": "$HOME",
  "agent": "claude",
  "mode": "bypassPermissions",
  "sync_listen": "",
  "sync_server": "",
  "sync_key": "~/.assistant/sync_ed25519",
  "slack_token_file": "~/.assistant/slack_token"
}
```

Every key also has a flag that overrides the file for one run: `-port`, `-db`, `-claude`,
`-claude-args`, `-gemini`, `-gemini-args`, `-workdir`, `-agent`, `-mode`, `-sync-listen`, `-sync-server`, `-sync-key`, `-slack-token-file`, plus `-config` to point at another file. Avoid ports
6665–6669: browsers refuse them.

Agents run with your user's permissions. The server only binds to 127.0.0.1 and rejects
non-JSON writes, foreign `Origin`s and foreign `Host` headers so that web pages can't drive it.

`sync_listen` (e.g. `127.0.0.1:6768`) makes this node the sync hub: a second listener serves
only `/sync/*` (push, pull, lease, snapshot, hello), and only to requests signed by a key in
`sync_keys`. The UI, API and MCP stay on the loopback port above. Empty (the default): no hub.

`sync_server` (e.g. `https://assistant.example.com`, no path) keeps this node in sync with the
hub there, signing with the Ed25519 key at `sync_key` (created on first start; its public half
is logged). Local changes are pushed about 2 s after the last one, the hub's are pulled as
they come (long-poll), and id blocks are leased before they run out. Offline, the app works as
usual and changes wait in the outbox; retries back off up to a minute. A node is either the
hub or syncs with one, not both. Empty (the default): no sync.

Setting sync up, with the app running or not (restart it afterwards, `make restart`):

```sh
# each node: make its key, set sync_server, print the public key
assistant sync init --server https://assistant.example.com
# the hub: accept that key as the next node (2, 3, ...); revoke NAME and keys too
assistant sync authorize <pubkey> --name laptop
# the first node, with your data: backs the database up (as make backup), sends all of it to the blank hub
assistant sync seed
# every other node, with an empty database: fills it from the hub's snapshot, refuses one with data
assistant sync join
```

A seed cut short goes on where it stopped when run again. Until a node is seeded or joined, a
set `sync_server` syncs nothing (the log says so).

## MCP server

The app is also an MCP server, so any Claude Code session (including the agents it starts)
can read and change the board: `http://127.0.0.1:6767/mcp` (Streamable HTTP). Register it once
for all your sessions:

```sh
claude mcp add --scope user --transport http assistant http://127.0.0.1:6767/mcp
```

Tools cover the whole app:

- **Tasks**: `get_board`, `get_task`, `search_tasks`, `create_task`, `update_task`, `move_task`,
  `archive_task`, `unarchive_task`, `list_archived_tasks`, `delete_task`, `list_kinds`, `add_kind`,
  `delete_kind`
- **Lanes & projects**: `create_lane`, `update_lane`, `delete_lane`, `create_project`,
  `update_project`, `delete_project`
- **Sessions**: `start_session`, `list_sessions`, `get_session`, `send_message`,
  `summarize_session`, `dismiss_needs_input`
- **Skills**: `list_skills`, `create_skill`, `update_skill`, `delete_skill`, `run_skill`
- **Memory**: `list_memories`, `search_memories`, `get_memory`, `create_memory`,
  `update_memory`, `delete_memory`, `remember`, `update_memories_from_session`

Each one goes through the JSON API, so validation and transition rules work as in the UI. The
app must be running (`make start`).

## Prompt templates

Rules, schedules and skills use Go `text/template`:

- `{{.Context}}` — the task brief: title, description, fields and links, plus what the tasks it
  builds on found out and its linked memories
- `{{.Task.Title}}`, `{{.Task.Description}}`, `{{.Task.ID}}`
- `{{.Fields.pull_request}}`, `{{.Fields.productive}}`, … (any custom field key)

Example rule prompt: `/p-dev:review {{.Fields.pull_request}}`

## Development

```sh
make test   # gofmt + vet + race tests; CI runs the same on every push
```

`main` is merged into only when the suite is green. See `CLAUDE.md` for layout and conventions.
