#!/usr/bin/env bash
# tg-agent-gateway installer
#
# Builds the Go gateway, installs the Node bridge and its SDKs, writes the
# config, installs the systemd unit and starts it. Safe to re-run: an existing
# config and session state are kept.
#
#   sudo ./installer.sh --token <bot token> --chat -1001234567890 --users 123456789
#   sudo ./installer.sh                       # re-install / upgrade in place
#   sudo ./installer.sh --uninstall [--purge]
set -euo pipefail

TOKEN="${TELEGRAM_BOT_TOKEN:-}"
CHAT="${TELEGRAM_CHAT_ID:-}"
USERS="${ALLOWED_USER_IDS:-}"
CWD="${DEFAULT_CWD:-/root}"
ROOTS="${WORKSPACE_ROOTS:-}"
ROOTS_GIVEN=0
[ -n "$ROOTS" ] && ROOTS_GIVEN=1
NO_START=0
UNINSTALL=0
PURGE=0
IMPORT_OLD="${IMPORT_OLD:-}"

# Overridable so a second instance, or a dry run, can live elsewhere.
BIN="${BIN:-/usr/local/bin/tg-agent-gateway}"
APPDIR="${APPDIR:-/opt/tg-agent-gateway}"
CONFIG_DIR="${CONFIG_DIR:-/etc/tg-agent-gateway}"
CONFIG="$CONFIG_DIR/config.json"
DATA_DIR="${DATA_DIR:-/var/lib/tg-agent-gateway}"
UNIT="${UNIT:-/etc/systemd/system/tg-agent-gateway.service}"
GO_MIN=1.24
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --chat) CHAT="$2"; shift 2 ;;
    --users) USERS="$2"; shift 2 ;;
    --cwd) CWD="$2"; shift 2 ;;
    --roots) ROOTS="$2"; ROOTS_GIVEN=1; shift 2 ;;
    --import-old) IMPORT_OLD="$2"; shift 2 ;;
    --no-start) NO_START=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    --purge) PURGE=1; shift ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "run as root"

if [ "$UNINSTALL" -eq 1 ]; then
  systemctl disable --now tg-agent-gateway 2>/dev/null || true
  rm -f "$UNIT"; systemctl daemon-reload
  rm -f "$BIN" /usr/local/bin/tg-send; rm -rf "$APPDIR"
  if [ "$PURGE" -eq 1 ]; then rm -rf "$CONFIG_DIR" "$DATA_DIR"; log "config and state removed"; fi
  log "uninstalled"
  exit 0
fi

# ---------------------------------------------------------------- packages
log "checking prerequisites"
if ! command -v node >/dev/null || [ "$(node -e 'console.log(process.versions.node.split(".")[0])')" -lt 20 ]; then
  log "installing Node 22"
  curl -fsSL https://deb.nodesource.com/setup_22.x | bash - >/dev/null
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nodejs >/dev/null
fi
command -v node >/dev/null || die "node is required"
command -v npm  >/dev/null || die "npm is required"

go_ok() {
  local v; v="$("$1" version 2>/dev/null | sed -n 's/^go version go\([0-9]*\.[0-9]*\).*/\1/p')" || return 1
  [ -n "$v" ] && [ "$(printf '%s\n%s\n' "$GO_MIN" "$v" | sort -V | head -1)" = "$GO_MIN" ]
}
GO=""
for cand in "$(command -v go 2>/dev/null || true)" /usr/local/go/bin/go; do
  if [ -n "$cand" ] && go_ok "$cand"; then GO="$cand"; break; fi
done
if [ -z "$GO" ]; then
  log "installing Go"
  arch="$(uname -m)"; case "$arch" in x86_64) goarch=amd64;; aarch64|arm64) goarch=arm64;; *) die "unsupported arch $arch";; esac
  ver="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)"
  tmp="$(mktemp -d)"; curl -fsSL -o "$tmp/go.tgz" "https://go.dev/dl/${ver}.linux-${goarch}.tar.gz"
  rm -rf /usr/local/go && tar -C /usr/local -xzf "$tmp/go.tgz" && rm -rf "$tmp"
  GO=/usr/local/go/bin/go
fi
log "using $("$GO" version)"

# Any one of these is enough; the gateway offers whichever it finds.
FOUND_AGENTS=""
for bin in claude codex agy; do
  if command -v "$bin" >/dev/null; then FOUND_AGENTS="$FOUND_AGENTS $bin"; fi
done
if [ -n "$FOUND_AGENTS" ]; then
  log "agents found:$FOUND_AGENTS"
else
  warn "none of claude, codex or agy is on PATH: install at least one, or sessions cannot start"
fi

# ---------------------------------------------------------------- build
log "building the gateway"
VERSION="$(git -C "$SRC" describe --tags --always --dirty 2>/dev/null || date +%Y%m%d)"
( cd "$SRC" && CGO_ENABLED=0 GOFLAGS=-mod=mod "$GO" build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$BIN.new" . )
install -m 0755 "$BIN.new" "$BIN"; rm -f "$BIN.new"
log "installed $BIN ($VERSION)"

log "installing the agent bridge"
mkdir -p "$APPDIR/bridge"
install -m 0644 "$SRC/bridge/index.mjs" "$APPDIR/bridge/index.mjs"
install -m 0644 "$SRC/bridge/worker.mjs" "$APPDIR/bridge/worker.mjs"
install -m 0644 "$SRC/bridge/package.json" "$APPDIR/bridge/package.json"
[ -f "$SRC/bridge/package-lock.json" ] && install -m 0644 "$SRC/bridge/package-lock.json" "$APPDIR/bridge/package-lock.json"
( cd "$APPDIR/bridge" && npm install --omit=dev --no-fund --no-audit --silent )

# tg-send: how an agent delivers a file into its own topic.
install -m 0755 "$SRC/tools/tg-send" /usr/local/bin/tg-send

# ---------------------------------------------------------------- config

# ask VAR "prompt" "default" -- reads an answer, keeping the default on Enter.
ask() {
  local __var="$1" __prompt="$2" __default="${3:-}" __reply=""
  if [ -n "$__default" ]; then
    read -r -p "$__prompt [$__default]: " __reply || true
    [ -n "$__reply" ] || __reply="$__default"
  else
    read -r -p "$__prompt: " __reply || true
  fi
  printf -v "$__var" '%s' "$__reply"
}

tg() { # tg <method> [curl args...] -- returns the raw JSON
  local method="$1"; shift
  curl -s --max-time 30 "https://api.telegram.org/bot${TOKEN}/${method}" "$@"
}

json() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(eval(sys.argv[1],{},{"d":d}) if d.get("ok") else "")' "$1" 2>/dev/null; }

# Ask for the token until Telegram accepts it, so a typo is caught here and
# not at the first message.
ask_token() {
  while :; do
    [ -n "$TOKEN" ] || ask TOKEN "Bot token from @BotFather"
    [ -n "$TOKEN" ] || { warn "a token is required"; continue; }
    local name
    name="$(tg getMe | json 'd["result"]["username"]')"
    if [ -n "$name" ]; then
      log "the bot is @$name"
      BOT_NAME="$name"
      return
    fi
    warn "Telegram did not accept that token"
    TOKEN=""
    [ -t 0 ] || die "no token, and nothing to ask"
  done
}

# Find the group and your user id by watching for a message, which is far
# easier than hunting for ids by hand.
discover_chat() {
  if systemctl is-active --quiet tg-agent-gateway 2>/dev/null; then
    log "the gateway is already running, so I will not read its updates; enter the ids by hand"
    return 1
  fi
  echo
  log "Now, in Telegram:"
  echo "    1. create a group, open its settings and turn Topics on"
  echo "    2. add @${BOT_NAME:-your bot} to it and make it an administrator with Manage Topics"
  echo "    3. send any message in that group"
  echo
  read -r -p "Press Enter once you have sent that message (or type 'skip'): " __go || true
  [ "$__go" = "skip" ] && return 1

  local found="" i
  for i in $(seq 1 20); do
    found="$(tg getUpdates -d offset=-20 -d timeout=2 | python3 -c '
import json, sys
d = json.load(sys.stdin)
if not d.get("ok"):
    raise SystemExit
for u in reversed(d.get("result", [])):
    m = u.get("message") or u.get("edited_message") or {}
    chat = m.get("chat") or {}
    if chat.get("type") in ("group", "supergroup") and m.get("from"):
        print(chat["id"], m["from"]["id"], int(bool(chat.get("is_forum"))),
              (chat.get("title") or "").replace("\n", " "), sep="\t")
        break
' 2>/dev/null)"
    [ -n "$found" ] && break
    printf '.'
    sleep 2
  done
  echo
  [ -n "$found" ] || { warn "no group message arrived"; return 1; }

  local chat_id user_id is_forum title
  chat_id="$(echo "$found" | cut -f1)"
  user_id="$(echo "$found" | cut -f2)"
  is_forum="$(echo "$found" | cut -f3)"
  title="$(echo "$found" | cut -f4-)"
  log "found the group \"$title\" ($chat_id), and you are $user_id"
  [ "$is_forum" = "1" ] || warn "Topics are not enabled in that group: turn them on, or each session cannot get its own topic"

  local yn=""
  ask yn "Use this group and account?" "yes"
  case "$yn" in
    y|Y|yes|YES|"") CHAT="$chat_id"; USERS="$user_id"; return 0 ;;
    *) return 1 ;;
  esac
}

check_admin() {
  [ -n "$CHAT" ] || return 0
  local me rights
  me="$(tg getMe | json 'd["result"]["id"]')"
  [ -n "$me" ] || return 0
  rights="$(tg getChatMember -d chat_id="$CHAT" -d user_id="$me" | python3 -c '
import json, sys
d = json.load(sys.stdin)
r = d.get("result", {}) if d.get("ok") else {}
print(r.get("status", "?"), int(bool(r.get("can_manage_topics"))), sep="\t")
' 2>/dev/null)"
  local status manage
  status="$(echo "$rights" | cut -f1)"
  manage="$(echo "$rights" | cut -f2)"
  if [ "$status" != "administrator" ]; then
    warn "the bot is not an administrator of that group; it will not be able to create topics"
  elif [ "$manage" != "1" ]; then
    warn "the bot is an administrator but without Manage Topics; it will not be able to create topics"
  else
    log "the bot is an administrator with Manage Topics"
  fi
  warn "also set /setprivacy to Disabled in @BotFather, or the bot only sees commands"
}

mkdir -p "$CONFIG_DIR" "$DATA_DIR"; chmod 700 "$DATA_DIR"
if [ ! -f "$CONFIG" ]; then
  if [ -t 0 ]; then
    echo
    log "Setting up. Press Enter to accept a default in [brackets]."
    ask_token
    if [ -z "$CHAT" ] || [ -z "$USERS" ]; then
      discover_chat || true
    fi
    while [ -z "$CHAT" ]; do
      ask CHAT "Group id (looks like -1001234567890)"
      case "$CHAT" in
        -*) ;;
        *) warn "a group id starts with a minus"; CHAT="" ;;
      esac
    done
    while [ -z "$USERS" ]; do
      ask USERS "Telegram user ids allowed to use it, comma separated"
      case "$USERS" in
        *[!0-9,\ ]*) warn "user ids are numbers"; USERS="" ;;
      esac
    done
    ask CWD "Folder new sessions start in" "$CWD"
    if [ "$ROOTS_GIVEN" -eq 0 ]; then
      ROOTS=""
    fi
    ask ROOTS "Folders sessions may work in, comma separated" "${ROOTS:-$CWD}"
    check_admin
  else
    [ -n "$TOKEN" ] || die "no config and no terminal: pass --token, --chat and --users"
    [ -n "$CHAT" ]  || die "no config and no terminal: pass --chat"
    [ -n "$USERS" ] || die "no config and no terminal: pass --users"
  fi
  [ -n "$ROOTS" ] || ROOTS="$CWD"
  "$BIN" -config "$CONFIG" init -token "$TOKEN" -chat "$CHAT" -users "$USERS" \
      -cwd "$CWD" -roots "$ROOTS" -bridge "$APPDIR/bridge/index.mjs"
  log "wrote $CONFIG"
else
  log "keeping existing $CONFIG"
  python3 - "$CONFIG" "$APPDIR/bridge/index.mjs" <<'PY'
import json, sys
path, bridge = sys.argv[1], sys.argv[2]
cfg = json.load(open(path))
cfg["bridge_cmd"] = ["node", bridge]
json.dump(cfg, open(path, "w"), indent=2)
PY
fi
chmod 600 "$CONFIG"

# One-off import from the older Python gateway: keep its topics and let each
# conversation resume where it left off.
STATE="$DATA_DIR/state.json"
if [ -n "$IMPORT_OLD" ] && [ -f "$IMPORT_OLD" ] && [ ! -s "$STATE" ]; then
  log "importing sessions from $IMPORT_OLD"
  python3 - "$IMPORT_OLD" "$STATE" <<'PY'
import json, sqlite3, sys, time, datetime
src, dst = sys.argv[1], sys.argv[2]
db = sqlite3.connect(src)
cols = [r[1] for r in db.execute("PRAGMA table_info(sessions)")]
topics = {}
for row in db.execute("select * from sessions"):
    r = dict(zip(cols, row))
    tid = int(r.get("thread_id") or 0)
    when = datetime.datetime.fromtimestamp(r.get("updated_at") or time.time(),
                                           datetime.timezone.utc).isoformat().replace("+00:00", "Z")
    topics[str(tid)] = {
        "thread_id": tid,
        "title": "Claude Code · " + (r.get("cwd") or "/").rstrip("/").split("/")[-1],
        "agent": "claude",
        "cwd": r.get("cwd") or "/root",
        "model": r.get("model") or "",
        "effort": "",
        "ref": r.get("claude_session_id") or "",
        "verbose": bool(r.get("show_thinking")),
        "cost": float(r.get("cost_usd") or 0),
        "turns": int(r.get("turns") or 0),
        "created": when,
        "last_used": when,
    }
json.dump({"topics": topics}, open(dst, "w"), indent=2)
print("imported", len(topics), "session(s)")
PY
  chmod 600 "$STATE"
fi

# ---------------------------------------------------------------- service
cat > "$UNIT" <<EOF
[Unit]
Description=Telegram agent gateway (Claude Code + Codex)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=$APPDIR
ExecStart=$BIN -config $CONFIG serve
Restart=always
RestartSec=3
# HOME must be root's: both agents authenticate from ~/.claude and ~/.codex.
Environment=HOME=/root
Environment=USER=root
Environment=LOGNAME=root
Environment=LANG=C.UTF-8
Environment=PATH=/root/.local/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
MemoryMax=8G
TasksMax=4096
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable tg-agent-gateway >/dev/null 2>&1 || true

# Two bots must never poll the same token.
for other in claude-tg-gateway; do
  if systemctl is-active --quiet "$other" 2>/dev/null; then
    warn "stopping $other: it polls the same bot token"
    systemctl disable --now "$other" || true
  fi
done

if [ "$NO_START" -eq 0 ]; then
  log "starting"
  systemctl restart tg-agent-gateway
  sleep 2
  systemctl is-active --quiet tg-agent-gateway || { journalctl -u tg-agent-gateway -n 20 --no-pager >&2; die "failed to start"; }
  log "running"
fi

"$BIN" -config "$CONFIG" check || true
echo
log "logs: journalctl -u tg-agent-gateway -f"
log "open the group and send /new"
