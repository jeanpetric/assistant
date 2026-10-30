# Slack: a task connected to a channel

A task in the assistant can be connected to one Slack channel. From then on:

- every message in the channel, top-level and in its threads, comes to the task's activity
  thread under its author ("Ana (Slack)") and goes to the task's **orchestrator** session as its
  next message (queued while it works);
- every message posted on the task's thread, by a session or by you in the assistant, goes to
  the channel (yours marked "_(iz assistenta)_", a question with "❓");
- the orchestrator hands each request to a sub-agent (a session on a sub-task that reports back
  to it with `report_to`) and posts the result in the request's Slack thread.

Writing in Slack or on the task's thread is the same, both ways.

```
Slack channel ──poll every ~5 s──▶ task thread ──▶ orchestrator session
     ▲                                                 │ create_task + start_session(report_to)
     │                                                 ▼
     └──chat.postMessage◀── post_task_message ◀── "Session #N … finished/failed/asks"
                                                   from the sub-agent's session
```

There is no public endpoint, Socket Mode or websocket: the app polls the Slack Web API with a
bot token. The Slack tables are local (not synced): a channel is read by the computer it is
connected on, and a hub never polls.

## 1. The Slack app

Once per workspace.

1. <https://api.slack.com/apps> → **Create New App** → **From an app manifest**, pick the
   workspace and paste `deploy/slack-manifest.yml`. It asks only for these bot scopes:

   | Scope | For |
   |---|---|
   | `channels:history`, `groups:history` | reading messages and thread replies of public / private channels |
   | `channels:read`, `groups:read` | the channel's name and whether the bot is in it (`conversations.info`) |
   | `users:read` | authors' names |
   | `chat:write` | posting the task's thread to the channel |

   Nothing else (no reactions, files, events or Socket Mode).
2. **Install to Workspace**, then copy the **Bot User OAuth Token** (`xoxb-…`) from
   *OAuth & Permissions*.
3. In each channel you will connect, invite the bot: `/invite @JeanBot`.

## 2. The token

```sh
printf '%s' 'xoxb-…' > ~/.assistant/slack_token
chmod 600 ~/.assistant/slack_token
```

That is the default path; another goes in `slack_token_file` in `~/.assistant/config.json` or
`-slack-token-file`. A missing or empty file leaves Slack off: connecting answers
"Slack is off: no bot token". The token is read at start, so restart the app after changing
it. Never commit or log it.

## 3. Connecting and disconnecting

**UI:** open the task → *Slack* → paste the channel's id (`C…`) or link
(`https://….slack.com/archives/C…`) → **Connect**. With several sessions of the task on this
computer you pick the orchestrator (default: the latest). A task without one gets a new
session, "Slack #channel", started to wait for the channel's messages. Once connected the
section shows `#channel` (a link to it), the sync status and the orchestrator, and
**Disconnect**; the card shows `#channel`, red while syncing fails.

**API / MCP:**

```sh
curl -X POST localhost:6767/api/tasks/42/slack -H 'Content-Type: application/json' \
     -d '{"channel": "C0CDK1XJUL9", "session_id": 7}'      # session_id optional
curl -X DELETE localhost:6767/api/tasks/42/slack
```

MCP: `connect_slack_channel {task_id, channel, session_id?}`, `disconnect_slack_channel
{task_id}`. The task's JSON has `slack: {channel_id, channel_name, session_id, url,
last_sync_at, error}` or `null`.

- Only messages after connecting come in; the channel's history stays out.
- A channel is connected to one task at a time (409 otherwise); connecting a connected task
  to another channel replaces it, to the same one only changes the orchestrator.
- Refused: a channel the bot can't see or is not in (400), a session of another task (400) or
  of another computer (409).
- **Disconnecting** stops polling and posting. The sessions, sub-tasks and the task's thread
  stay; reconnecting later takes in only what comes after.

## 4. The orchestrator

The orchestrator is the session in `slack_channels.session_id`. On each turn its system prompt
gets, after the usual activity-thread note, the orchestrator note (`OrchestratorNote` in
`internal/app/slack_sync.go`):

- messages starting with `[Slack #channel]` come from people in the channel, a line each:
  `Name — task_message N, ts T, thread R: text`;
- for each new request it creates a sub-task (`create_task`, same project, `context_from` the
  task if needed) and starts a session on it with `report_to` = its own session id, posts in
  the request's thread that the work has started, and ends its turn;
- short questions and clarifications it answers itself;
- when "Session #N … finished:" / "failed:" / "asks:" comes, it posts the resolution with
  `post_task_message` and `reply_to` the original request; a sub-agent's question goes to the
  same thread and the answer back with `send_message`;
- it replies in threads; at the top of the channel (`top_level`) only when someone asks.

Where a message goes: `reply_to` (a task message from the channel: its Slack thread), or
`top_level`; with neither, a session's message goes to the thread of the latest Slack message
delivered to it, else to the top.

## 5. While the app is off

Nothing is lost. The channel's cursor and each followed thread's last reply are in the
database; on the next start the first poll takes in everything since (all top-level messages
after the cursor, page by page, and new replies of the threads it follows) and hands them to
the orchestrator as one message. A session that was running when the app stopped is
interrupted; the orchestrator is resumed by the next message.

If the orchestrator can't take them (deleted, another computer's, or none), the messages wait
on the task's thread, the Slack status shows why and the first such poll notifies you; connect
again with a session to go on. A failed post stays on the thread, shows in the status and
notifies once; it is not retried.

## 6. Limits

- **Latency:** polling every ~5 s, plus the orchestrator's turn; replies to a thread show up
  only once its root message is known (posted after connecting).
- **Rate limits:** a poll is one `conversations.history` per channel plus
  `conversations.replies` only for threads with new replies, at most 10 a poll (the rest wait
  for the next). On a 429 the client waits `Retry-After` and retries once; still limited, the
  poll fails and shows in the status.
- The bot's own messages and system messages (joins, topic …) are skipped; `<@U…>` becomes
  `@Name`. Files, reactions and edits are not taken in.
- A message over 12,000 characters goes out in parts, the rest in the first one's thread.
- No allowlist: anyone in the channel can give the orchestrator work, which runs with the
  app's permission mode. Connect only channels whose members you trust.

## 7. Manual check

On a test instance, never your own: a worktree build on another port, with its own config
(no Trello, no sync), an empty DB and a workdir in `/tmp`, the real token file.

```sh
mkdir -p /tmp/slack-e2e/work
echo '{"workdir": "/tmp/slack-e2e/work"}' > /tmp/slack-e2e/config.json
go build -o /tmp/slack-e2e/assistant ./cmd/assistant
/tmp/slack-e2e/assistant -config /tmp/slack-e2e/config.json -db /tmp/slack-e2e/test.db -port 6799
```

1. [ ] Create a test task; connect it to the test channel without a session: an orchestrator
       session "Slack #…" starts, the dialog shows `#channel`, "Synced …".
2. [ ] Write a request in the channel (as a person, not the bot): it shows on the task's thread
       as "Name (Slack)", the orchestrator gets it, creates a sub-task with a session reporting
       to it, and says in the request's thread that work started.
3. [ ] The sub-session finishes: the orchestrator posts the result in the same thread.
4. [ ] A request that needs a decision: the sub-agent asks, the question shows in the thread,
       your answer there goes back to the sub-agent, and the result follows.
5. [ ] "Write X at the top of the channel": it is posted top-level.
6. [ ] A message from the assistant UI on the task's thread shows in the channel, marked.
7. [ ] Disconnect: new channel messages no longer come to the task, nothing more is posted.
