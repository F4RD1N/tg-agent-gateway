// Agent bridge.
//
// The gateway talks to this process over stdin/stdout in newline-delimited
// JSON. It keeps one worker process per session, each in its own process
// group, so a session can be killed outright - together with every command
// its agent spawned - without disturbing the others.
//
//   in :  {"type":"start","sid":"42","agent":"claude","cwd":"/root",...}
//         {"type":"prompt","sid":"42","text":"hello"}
//         {"type":"interrupt","sid":"42"}   graceful: end the turn
//         {"type":"kill","sid":"42"}        hard: SIGKILL the process group
//         {"type":"stop","sid":"42"}        close the session
//         {"type":"models","sid":"m1","agent":"codex"}   {"type":"ping"}
//   out:  started | delta | text | thinking | tool | file | todo | busy |
//         idle | stopped | killed | done | models | error | pong
import readline from 'node:readline';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { query } from '@anthropic-ai/claude-agent-sdk';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const WORKER = path.join(HERE, 'worker.mjs');

let pendingWork = 0; // async work that must finish before the process may exit

function out(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}

function childEnv() {
  return { ...process.env, IS_SANDBOX: '1' };
}

// ---------------------------------------------------------------- workers

const workers = new Map(); // sid -> worker record

function worker(sid) {
  let w = workers.get(sid);
  if (w && w.proc && w.proc.exitCode === null && !w.proc.killed) return w;

  const proc = spawn(process.execPath, [WORKER, sid], {
    cwd: HERE,
    env: childEnv(),
    stdio: ['pipe', 'pipe', 'pipe'],
    // Its own process group: killing -pid takes the agent and everything it
    // started (bash, compilers, servers) with it.
    detached: true,
  });
  const rec = { proc, buf: '', running: false, last: '', config: w?.config || null };
  workers.set(sid, rec);

  proc.stdout.on('data', chunk => {
    rec.buf += chunk;
    let i;
    while ((i = rec.buf.indexOf('\n')) >= 0) {
      const line = rec.buf.slice(0, i);
      rec.buf = rec.buf.slice(i + 1);
      if (!line.trim()) continue;
      let ev;
      try { ev = JSON.parse(line); } catch { continue; }
      if (ev.type === 'idle') { rec.running = false; continue; }
      if (ev.type === 'started' || ev.type === 'busy') rec.running = true;
      if (ev.type === 'done') rec.running = false;
      if (ev.session) rec.last = ev.session;
      ev.sid = sid;
      out(ev);
    }
  });
  proc.stderr.on('data', d => {
    const text = String(d).trim();
    if (text) process.stderr.write(`[worker ${sid}] ${text}\n`);
  });
  proc.on('exit', (code, signal) => {
    const wasRunning = rec.running;
    if (workers.get(sid) === rec) workers.delete(sid);
    if (rec.killedOnPurpose) {
      out({ type: 'killed', sid, message: 'killed the agent and everything it was running' });
      if (wasRunning) out({ type: 'done', sid, session: rec.last, subtype: 'killed', duration_ms: 0 });
      return;
    }
    if (wasRunning) {
      out({ type: 'error', sid, message: `the agent process ended (${signal || code}); the next message starts a fresh one` });
      out({ type: 'done', sid, session: rec.last, subtype: 'error', duration_ms: 0 });
    }
  });
  return rec;
}

function send(sid, msg) {
  const w = worker(sid);
  if (msg.type === 'start') {
    w.config = msg;
  } else if (msg.type === 'prompt' && w.config) {
    // A worker that was killed and respawned needs its settings back.
    for (const k of Object.keys(w.config)) {
      if (k !== 'type' && k !== 'text' && msg[k] === undefined) msg[k] = w.config[k];
    }
  }
  try {
    w.proc.stdin.write(JSON.stringify(msg) + '\n');
  } catch (err) {
    out({ type: 'error', sid, message: 'could not reach the agent process: ' + String(err?.message || err) });
  }
}

function killSession(sid, close) {
  const w = workers.get(sid);
  if (!w || !w.proc || w.proc.exitCode !== null) {
    if (close) workers.delete(sid);
    out({ type: 'killed', sid, message: 'nothing was running' });
    return;
  }
  w.killedOnPurpose = true;
  w.closing = !!close;
  try {
    // Negative pid: the whole process group, so the agent's own children die
    // with it instead of being reparented and left behind.
    process.kill(-w.proc.pid, 'SIGKILL');
  } catch {
    try { w.proc.kill('SIGKILL'); } catch { /* already gone */ }
  }
}

// ---------------------------------------------------------------- models
//
// The gateway shows the real list each agent supports, with the effort levels
// that model accepts, so the buttons in Telegram are never a stale copy.

const modelCache = new Map(); // agent -> { at, models }
const MODEL_TTL = 10 * 60 * 1000;

async function listModels(agent) {
  const hit = modelCache.get(agent);
  if (hit && Date.now() - hit.at < MODEL_TTL) return hit.models;
  const models = agent === 'codex' ? codexModels() : await claudeModels();
  modelCache.set(agent, { at: Date.now(), models });
  return models;
}

// Claude answers control requests only in streaming-input mode, so this opens
// a query whose iterator stays open, asks, and shuts it down again. No prompt
// is ever sent, so the lookup costs nothing.
async function claudeModels() {
  const ac = new AbortController();
  let release;
  const gate = new Promise(r => { release = r; });
  async function* held() { await gate; }
  const q = query({
    prompt: held(),
    options: {
      cwd: process.cwd(),
      permissionMode: 'bypassPermissions',
      allowDangerouslySkipPermissions: true,
      settingSources: [],
      abortController: ac,
      env: childEnv(),
    },
  });
  try {
    const list = await q.supportedModels();
    return list.map(m => ({
      id: m.value,
      label: cleanLabel(m.displayName || m.value),
      description: m.description || '',
      efforts: m.supportsEffort ? (m.supportedEffortLevels || []) : [],
      default_effort: '',
    }));
  } finally {
    release();
    try { await q.return?.(); } catch { /* already closed */ }
    ac.abort();
  }
}

// cleanLabel drops the parenthetical noise Claude puts in display names, so
// a button reads "Default" rather than "Default (recommended)".
function cleanLabel(label) {
  return String(label)
    .replace(/\s*\((?:recommended|default)\)\s*/gi, ' ')
    .replace(/\s*\brecommended\b\s*/gi, ' ')
    .replace(/\s{2,}/g, ' ')
    .trim();
}

function codexModels() {
  const file = path.join(os.homedir(), '.codex', 'models_cache.json');
  let raw = null;
  try { raw = JSON.parse(fs.readFileSync(file, 'utf8')); } catch { /* not cached yet */ }
  const list = (raw?.models || [])
    .filter(m => m && m.slug && m.visibility !== 'hidden')
    .map(m => ({
      id: m.slug,
      label: cleanLabel(m.display_name || m.slug),
      description: m.description || '',
      efforts: (m.supported_reasoning_levels || []).map(e => (typeof e === 'string' ? e : e.effort)).filter(Boolean),
      default_effort: m.default_reasoning_level || '',
    }));
  const efforts = ['low', 'medium', 'high', 'xhigh'];
  if (!list.length) {
    return [
      { id: '', label: 'Default', description: 'whatever the CLI is set to', efforts: [], default_effort: '' },
      { id: 'gpt-6-astra', label: 'GPT-6-Astra', description: '', efforts, default_effort: 'medium' },
    ];
  }
  return [{ id: '', label: 'Default', description: 'whatever the CLI is set to', efforts: [], default_effort: '' }, ...list];
}

// ---------------------------------------------------------------- protocol

const rl = readline.createInterface({ input: process.stdin });
rl.on('line', line => {
  line = line.trim();
  if (!line) return;
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  try { handle(msg); } catch (err) { out({ type: 'error', sid: msg.sid || '', message: String(err?.message || err) }); }
});
rl.on('close', () => {
  const wait = () => {
    const busy = pendingWork > 0 || [...workers.values()].some(w => w.running);
    if (busy) { setTimeout(wait, 250); return; }
    for (const sid of [...workers.keys()]) killSession(sid, true);
    setTimeout(() => process.exit(0), 200);
  };
  wait();
});

function handle(msg) {
  switch (msg.type) {
    case 'ping':
      out({ type: 'pong' });
      break;
    case 'models': {
      pendingWork++;
      listModels(msg.agent === 'codex' ? 'codex' : 'claude')
        .then(models => out({ type: 'models', sid: msg.sid || '', agent: msg.agent || 'claude', models }))
        .catch(err => out({ type: 'error', sid: msg.sid || '', message: 'models: ' + String(err?.message || err) }))
        .finally(() => { pendingWork--; });
      break;
    }
    case 'kill':
      killSession(msg.sid, false);
      break;
    case 'stop':
      killSession(msg.sid, true);
      out({ type: 'ok', sid: msg.sid || '' });
      break;
    case 'start':
    case 'prompt':
    case 'interrupt':
    case 'clear':
      send(msg.sid, msg);
      break;
  }
}

process.on('uncaughtException', err => {
  out({ type: 'error', sid: '', message: 'bridge: ' + String(err?.message || err) });
});
process.on('unhandledRejection', err => {
  out({ type: 'error', sid: '', message: 'bridge: ' + String(err?.message || err) });
});

out({ type: 'hello', pid: process.pid, node: process.version });
