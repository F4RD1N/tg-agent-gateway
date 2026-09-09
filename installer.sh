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
ROOTS="${WORKSPACE_ROOTS:-/root}"
NO_START=0
UNINSTALL=0
PURGE=0
IMPORT_OLD="${IMPORT_OLD:-}"

BIN=/usr/local/bin/tg-agent-gateway
APPDIR=/opt/tg-agent-gateway
CONFIG_DIR=/etc/tg-agent-gateway
CONFIG="$CONFIG_DIR/config.json"
DATA_DIR=/var/lib/tg-agent-gateway
UNIT=/etc/systemd/system/tg-agent-gateway.service
GO_MIN=1.24
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --chat) CHAT="$2"; shift 2 ;;
    --users) USERS="$2"; shift 2 ;;
    --cwd) CWD="$2"; shift 2 ;;
    --roots) ROOTS="$2"; shift 2 ;;
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
  rm -f "$BIN"; rm -rf "$APPDIR"
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

for bin in claude codex; do
  command -v "$bin" >/dev/null || warn "$bin is not on PATH; that agent will fail until it is installed"
done

# ---------------------------------------------------------------- build
log "building the gateway"
VERSION="$(git -C "$SRC" describe --tags --always --dirty 2>/dev/null || date +%Y%m%d)"
( cd "$SRC" && CGO_ENABLED=0 GOFLAGS=-mod=mod "$GO" build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$BIN.new" . )
install -m 0755 "$BIN.new" "$BIN"; rm -f "$BIN.new"
log "installed $BIN ($VERSION)"

log "installing the agent bridge"
mkdir -p "$APPDIR/bridge"
install -m 0644 "$SRC/bridge/index.mjs" "$APPDIR/bridge/index.mjs"
install -m 0644 "$SRC/bridge/package.json" "$APPDIR/bridge/package.json"
[ -f "$SRC/bridge/package-lock.json" ] && install -m 0644 "$SRC/bridge/package-lock.json" "$APPDIR/bridge/package-lock.json"
( cd "$APPDIR/bridge" && npm install --omit=dev --no-fund --no-audit --silent )

# ---------------------------------------------------------------- config
mkdir -p "$CONFIG_DIR" "$DATA_DIR"; chmod 700 "$DATA_DIR"
if [ ! -f "$CONFIG" ]; then
  [ -n "$TOKEN" ] || read -r -p "Bot token from @BotFather: " TOKEN
  [ -n "$CHAT" ]  || read -r -p "Forum supergroup id (e.g. -1001234567890): " CHAT
  [ -n "$USERS" ] || read -r -p "Your Telegram user id: " USERS
  "$BIN" -config "$CONFIG" init -token "$TOKEN" -chat "$CHAT" -users "$USERS" \
      -cwd "$CWD" -roots "$ROOTS" -bridge "$APPDIR/bridge/index.mjs"
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
