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
- **Many topics at once.** Sessions run in parallel; the edit pacing widens
  automatically as more of them stream, so they do not trip Telegram's
  group-wide flood limit.
- **It manages the topics.** It creates, renames, closes and deletes them,
  and on startup it gives every remembered session a topic again, recreating
  any that were deleted while it was down.
- **Photos and files both ways.** Send a photo with a caption and the agent
  looks at the picture and does what the caption says; Codex is handed the
  image directly, Claude opens it with its Read tool. An album of photos
  arrives as one message. Any file lands in the session's folder, and
  `/get path` sends one back. Agents deliver
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
sudo ./installer.sh --token <bot token> --chat -1001234567890 --users <your user id>
```

`/id` in the group prints the chat and user ids. The installer adds Node and
Go if they are missing, builds the binary, installs the bridge with its SDKs,
writes `/etc/tg-agent-gateway/config.json`, installs the service and starts it.

Other options: `--cwd` (default folder), `--roots` (folders sessions may use),
`--import-old <gateway.db>` (take over sessions from the older Python
gateway), `--no-start`, `--uninstall [--purge]`.

## Using it

In **General**:

| | |
|---|---|
| `/new` | pick agent and folder with buttons |
| `/new My Project` | same, with that topic name |
| `/new codex /root/app` | skip straight to a session |
| `/sessions` | list them, with a button that opens each topic |
| `/help` `/id` | |

In a **topic**:

| | |
|---|---|
| any message | a prompt for that session |
| `/model` | the agent's full model list, then its effort levels |
| `/mode` | accept edits, plan, don't ask, auto, manual |
| `/skills` | run a skill, plugin command or prompt file |
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
| `/end` | close or delete the topic |
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

## Development

```bash
go test ./...                                  # fake Telegram + fake bridge, no network
go test -race ./...                            # the same, checked for data races
AGENT_LIVE=1 go test -run TestLiveAgents -v    # real turns against all three agents
```

The Go tests run the whole update flow against an in-process Bot API stub, so
button presses, topic creation, streaming edits, stopping and the pickers are
all covered without touching Telegram.

Two layout rules the tests enforce: no keyboard row holds more than two
buttons, and a message that carries buttons is padded to a minimum width, or
Telegram shrinks the bubble and clips the labels.

Layout: `main.go` (CLI), `gateway.go` (routing, sessions, streaming),
`steps.go` (tool calls to human phrases), `tools/tg-send` (delivery into a
topic),
`handlers.go` (commands and buttons), `telegram.go` (Bot API, keyboard
layout), `bridge.go` (sidecar protocol), `render.go` (markdown → Telegram
HTML), `store.go` (JSON state), `bridge/index.mjs` (worker supervision and
model lists), `bridge/worker.mjs` (one session, both agent SDKs).
