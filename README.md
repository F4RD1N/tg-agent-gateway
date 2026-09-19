# tg-agent-gateway

Run **Claude Code**, **Codex** and **Antigravity** from a Telegram group. Each forum topic is
one agent session: write in a topic and the agent works there, answering into
the same message as it streams. Sessions survive restarts, several topics run
at the same time, and every choice is made with inline ("glass") buttons.

```
Telegram forum group ──▶ gateway (Go) ──JSONL──▶ bridge (Node)
                              │                     ├─ @anthropic-ai/claude-agent-sdk
                              └─ topics, buttons     ├─ @openai/codex-sdk
                                 streaming, files    └─ agy (Antigravity CLI)
```

Written in Go and JavaScript: Go owns Telegram, the topics and the rendering;
a Node sidecar drives each agent through its **official SDK**, so there is no
CLI output to parse and conversations resume by their real session id. Each
session gets its own worker process in its own process group, so Kill really
does kill everything that session started.

## What it does

- **A topic per session.** `/new` in General asks for the agent, the folder
  and what to call the topic, all on buttons, then creates it. The name you
  give becomes "Claude • VPN App"; the agent stays in front when you rename
  it or switch agents. Send a message in a topic and it goes to that session.
- **Three agents, full access.** Claude Code runs with `bypassPermissions`,
  Codex with `danger-full-access` and no approval prompts, Antigravity with
  `--dangerously-skip-permissions`. Switch a topic between them with a button;
  each keeps its own conversation and resumes by its own id.
- **Modes on buttons.** `/mode` offers what the agent supports: accept edits,
  plan, don't ask, auto, manual, full access.
- **It recovers from a stopped thread.** Codex sometimes ends a conversation
  with "chat stopped as a precaution", and that thread can never be resumed.
  The gateway notices, starts a fresh thread and tells it to pick up the
  abandoned one's work, so the topic carries on. `/session` shows a topic's
  conversation id and `/resume <id>` points a topic at any conversation, which
  is the manual way back.
- **Load by UUID.** Send `/claude <uuid>`, `/codex <uuid>` or `/agy <uuid>`
  in General or any topic. The bot finds the session on this server, asks for
  a topic name, then opens it with that name. If already open, its existing
  topic is renamed and linked; otherwise a new topic is created. Older local
  history and kept sessions work too; kept settings, ownership and sandbox
  folders are preserved. Send `/cancel` or tap Cancel to abandon naming.
  These commands are for gateway administrators.
- **Skills and plugins.** `/skills` lists what the agent can be told to run by
  name — Claude's skills, plugin skills and slash commands, Antigravity's
  skills, Codex's prompt files — and runs it on a tap, asking for arguments
  when the skill takes them. Claude and Antigravity load the machine's own
  settings, so the skills and plugins already installed here just work.
- **The real model list.** The model button asks the agent what it supports
  and shows every model on a button; picking one then offers exactly the
  effort levels that model accepts (low … max, and Codex's ultra).
- **A Config button.** Everything `/config` would offer, on buttons and per
  session: for Claude the permission mode, thinking budget, turn limit, cost
  ceiling, *switch models when a message is flagged* (the refusal fallback)
  and whether `~/.claude/settings.json` is loaded; for Codex the sandbox,
  approval policy, web search and network access.
- **Live answers, the way app builders show them.** The reply is one message
  edited as it streams, and what it shows is the agent's own narration: one
  paragraph per thing it set out to do, ticked off as it moves on.

  ```
  ✅️ I'll set the project up and wire the pieces together.
  ✅️ The layout is in place. Adding the parts it depends on.
  ✳️ Everything is built. I'm checking that it works.
  ```

  The commands behind those sentences are not shown, and neither is their
  output: the agent already said what it is doing, in better words than a
  command line. Switch Details on for the list of steps, folded away.
  **Stop** ends the turn but leaves it resumable; **Kill** takes down the
  agent process and every command it started.
- **Write while it is working.** A message sent mid-turn is not lost and does
  not interrupt anything by surprise. The bot asks what to do with it:
  **Add to Queue** runs it when the current turn ends, **Stop and Apply** stops
  the turn and runs it now (taking the Stop and Kill buttons off the log it
  just ended), **Dismiss** forgets it. Queue as many as you like and they run
  in order; `/queue` lists them.
- **Ultracode, and the workflows it starts.** The effort picker for Claude
  models offers **Ultracode** alongside low … max: xhigh thinking plus
  multi-agent orchestration, so a big job is fanned out across a crowd of
  agents and verified rather than done alone. `/workflows` then shows the last
  seven runs in that folder; open one for its phases, its agents and its log,
  and a run still going **redraws itself** every few seconds until it ends.
- **Fast, for Codex.** `/fast` in a Codex topic switches it to Codex's
  priority tier: the same model served about twice as quickly, against more of
  the plan's usage. Three states, because this machine has a setting of its
  own: on, standard, or follow `service_tier` in `~/.codex/config.toml`.
- **Many topics at once.** Sessions run in parallel; the edit pacing widens
  automatically as more of them stream, so they do not trip Telegram's
  group-wide flood limit.
- **It manages the topics.** It creates, renames, closes and deletes them,
  and on startup it gives every remembered session a topic again, recreating
  any that were deleted while it was down. **Delete a topic but keep the
  session** and the conversation goes to the archive instead: `/sessions`
  finds it later and gives it a new topic where it carries on.
- **A session browser.** `/sessions` in General lists the agents; pick one and
  it shows that agent's last ten conversations, read from where the agent
  itself keeps them - so conversations that were never started here are
  listed too. Open one for a summary (what it was about, its id, its folder,
  how long ago) and a button that resumes it in a new topic.
- **Codex answer buttons.** A short question followed by 2–6 bullet or numbered
  options becomes a Telegram inline keyboard. Tap a choice to send its full text
  to that conversation; up to three questions are collected before sending.
  Answers selected while a turn is running queue automatically. You can still
  type a custom reply. Old, repeated, foreign-topic and unauthorized taps cannot
  submit an answer. Ordinary lists, quoted text and code do not become buttons.
- **Isolated sessions.** Instead of picking a folder, pick **Isolated
  sandbox**: the session gets a folder of its own under `/root/isolated` and
  sees nothing else of the machine. No other projects, no memories, no
  settings, no other session's history, and none of `/etc` that holds
  secrets - not this bot's own token, the panel database, the TLS keys or the
  password file. What it does have is a real machine: shell, compilers,
  package tools, the network, and root inside its own namespace. It cannot
  mount a disk, load a module, see another process or touch the services
  running outside. The bot asks who the sandbox is for; answer *someone else*
  and give their Telegram id, and that topic answers them and the gateway
  administrators. Administrators can use every session, including its settings,
  stop/kill controls and end/close/delete buttons.
- **Services instead of systemd.** There is no init inside a sandbox, so a
  website started from a chat message would die with the turn. `svc` is the
  stand-in: `svc start web npm run dev`, then `svc list`, `svc log web`,
  `svc stop web`. A service keeps running between messages, comes back if the
  session restarts, and the port it opens is reachable from outside. `/services`
  shows what is running.
- **Anything you send, with the caption as the prompt.** Photos, documents,
  video, audio, voice notes, animations, stickers - all of them land in the
  session's folder and the caption becomes the instruction. A picture is
  handed to the agent as an image (Codex takes it directly, Claude opens it
  with its Read tool); anything else is named for it. An album arrives as one
  message. **Send a file with no caption and the bot asks what to do with it**,
  holding the file until your next message, so an attachment alone never
  starts a turn. `/get path` sends a file back. Agents deliver
  with `tg-send <file>`, which the gateway points at that session's own topic
  through `AGENT_TG_*` in their environment, so builds and archives never go
  to a private chat again. `/run` executes a shell command in the folder.

## Install

Needs a Linux server with systemd, and whichever agents you want (`claude`,
`codex`, `agy`) logged in as the user the service runs as, and a Telegram **forum** supergroup where the bot is an
administrator with *Manage Topics*. In BotFather, set `/setprivacy` to
**Disabled** so the bot sees plain messages in the group.

```bash
unzip tg-agent-gateway_source_*.zip -d tg-agent-gateway && cd tg-agent-gateway
sudo ./installer.sh
```

It will ask for the token, the group, who may use it, which of those run it,
and which folders it may work in. It installs bubblewrap if it is missing, so
isolated sessions work out of the box. To skip the questions, pass them instead:

```bash
sudo ./installer.sh --token <bot token> --chat -1001234567890 --users <your user id>
```

`/id` in the group prints the chat and user ids. The installer adds Node and
Go if they are missing, builds the binary, installs the bridge with its SDKs,
writes `/etc/tg-agent-gateway/config.json`, installs the service and starts it.

Other options: `--cwd` (default folder), `--roots` (folders sessions may use),
`--admins` (who may browse and start sessions), `--isolated` (where sandboxes
live),
`--import-old <gateway.db>` (take over sessions from the older Python
gateway), `--no-start`, `--uninstall [--purge]`.

## Using it

If Codex repeatedly reports WebSocket disconnects before falling back to HTTPS,
see [the transport troubleshooting notes](CODEX_RECOVERY.md#websocket-disconnects).
The [HTTPS provider example](examples/codex-https.toml) uses the existing OpenAI
sign-in and model. Apply it to the service user's Codex config; the next CLI
request reads it without restarting the gateway.

In **General**:

| | |
|---|---|
| `/new` | pick agent and folder with buttons |
| `/new My Project` | same, with that topic name |
| `/new codex /root/app` | skip straight to a session |
| `/sessions` | browse past conversations and resume one |
| `/claude <uuid>` `/codex <uuid>` `/agy <uuid>` | find a session, ask for its topic name, then open it |
| `/help` `/id` | |

In a **topic**:

| | |
|---|---|
| any message | a prompt for that session |
| `/model` | the agent's full model list, then its effort levels |
| `/mode` | accept edits, plan, don't ask, auto, manual |
| `/skills` | run a skill, plugin command or prompt file |
| `/workflows` | the last seven workflow runs, with the live one updating itself |
| `/fast` | Codex only: its priority tier, the same model about twice as fast |
| `/queue` | messages waiting for the current turn to end |
| `/config` | permissions, thinking, limits, sandbox — all on buttons |
| `/agent` | switch between Claude Code and Codex |
| `/cd` `/pwd` `/ls` | working folder (`/cd` browses with buttons) |
| `/get <file>` `/run <cmd>` | fetch a file, run a shell command |
| `/stop` | interrupt the running turn (or press Stop) |
| `/kill` | kill the agent process and everything it started |
| `/clear` | forget the conversation, keep the topic |
| `/session` `/resume <id>` | show, or take over, a conversation id |
| `/compact` | compact the agent's context |
| `/rename <name>` | rename the topic |
| `/end` | close the topic, or delete it and keep the session |
| `/services` | what an isolated session keeps running |
| `/verbose` | show tool output and thinking |
| `/status` | what this session is, with its buttons |

Every command above is registered with Telegram, so typing `/` in the group
lists them. Anything else starting with `/` is passed to the agent, so its
own commands work too.

## Configuration

`/etc/tg-agent-gateway/config.json`:

| key | meaning |
|---|---|
| `bot_token` | from BotFather; treat it as root on this machine |
| `chat_id` | the forum supergroup |
| `allowed_user_ids` | who may use it; everyone else is ignored silently |
| `admin_user_ids` | of those, who has full gateway access, including sessions assigned to someone else (default: all allowed users) |
| `isolated_root` | where sandboxed sessions get their folders (`/root/isolated`) |
| `default_cwd` | folder offered for new sessions |
| `workspace_roots` | sessions and file transfers may not leave these |
| `state_path` | remembered sessions (`/var/lib/tg-agent-gateway/state.json`) |
| `bridge_cmd` | how to start the Node bridge |
| `edit_interval_ms` | base pacing for live edits; multiplied by the number of streaming topics |
| `max_message_chars` | where a long answer is sealed and continued in a new message |
| `show_thinking` `show_tools` | defaults for new sessions |
| `claude_models` `codex_models` `antigravity_models` `codex_efforts` | only a fallback: the live list comes from the agent |

## Command line

```
tg-agent-gateway serve      run the bot (what the service does)
tg-agent-gateway check      token, group, agents, bridge, sessions
tg-agent-gateway sessions   dump the remembered sessions as JSON
tg-agent-gateway init ...   write a config
```

## Security

Whoever is in `allowed_user_ids` gets an agent with full access to this
machine, so the bot token and the group are as sensitive as SSH keys.
Unknown users and other chats are dropped before any handler runs. The
prompt never becomes a shell string: it travels to the agent over stdin as
JSON. `/run` is the one deliberate shell path, and it is confined to the
session's folder.

**Isolated sessions** are how somebody else gets an agent here without
getting the machine. The whole session - the agent, the shell it runs, every
command those start - lives inside a bubblewrap sandbox whose only writable
place is the session's own folder, mounted at `/workspace`. `/root` does not
exist in there, `/etc` is rebuilt from the handful of files that programs
need, and the process, IPC and hostname namespaces are its own. `/run`,
`/get`, `/ls` and `/cd` in such a topic are confined to that folder too, and
the sandbox is never handed the bot token: files come back through the
session's outbox, which the gateway posts into the topic. A session made for
somebody else answers that Telegram id and gateway administrators. Other users
cannot act in it. Administrator access does not change the session's filesystem
sandbox.

Two things it deliberately keeps: the network, and each agent's own login
credential, without which no agent can run at all. So an isolated session can
reach the internet and spend that account's quota. It is a boundary against
reading this server, not a captive box.

**What Telegram cannot do:** hide a topic from a group member. Everyone in
the group sees every topic in the list. The bot enforces who may *act* -
guests only in their own topic, `/sessions` and `/new` only for admins - but
the titles are visible to all. Keep genuinely separate work in a separate
group.

## Development

The installed [Codex SDK](https://developers.openai.com/codex/sdk/) uses
`codex exec` JSONL. A live transport probe confirmed that
`request_user_input_async` currently arrives as a completed `agent_message`
containing a question and bullet options, without its structured question data.
`questions.go` recognizes that narrow format in Codex text events and sends
selections as ordinary conversation input. This is not an App Server tool-result
response or mid-turn steering; the [App Server protocol](https://developers.openai.com/codex/app-server/)
provides those separately. Questions without recognizable choices remain text.

```bash
go test ./...                                  # fake Telegram + fake bridge, no network
go test -race ./...                            # the same, checked for data races
AGENT_LIVE=1 go test -run TestLiveAgents -v    # real turns against all three agents
cd bridge && npm run lint                      # the sidecar, checked for undeclared names
```

The suite runs the linter itself, because `node --check` only sees syntax: a
name that was never declared throws at runtime, on whichever rare branch
reaches it, which is exactly how one bug reached a live session.

The Go tests run the whole update flow against an in-process Bot API stub, so
button presses, topic creation, streaming edits, stopping and the pickers are
all covered without touching Telegram.

Two layout rules the tests enforce: no keyboard row holds more than two
buttons, and a message that carries buttons is padded to a minimum width, or
Telegram shrinks the bubble and clips the labels.

Layout: `main.go` (CLI), `gateway.go` (routing, sessions, streaming),
`sandbox.go` (isolated sessions and their path boundary), `outbox.go`
(delivery out of a sandbox), `tools/sandbox-run` (the sandbox itself),
`tools/svc` (services inside one), `bridge/history.mjs` (past conversations),
`steps.go` (tool calls to human phrases), `tools/tg-send` (delivery into a
topic),
`handlers.go` (commands and buttons), `telegram.go` (Bot API, keyboard
layout), `bridge.go` (sidecar protocol), `render.go` (markdown → Telegram
HTML), `store.go` (JSON state), `bridge/index.mjs` (worker supervision and
model lists), `bridge/worker.mjs` (one session, both agent SDKs).
