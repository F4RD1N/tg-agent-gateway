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
import { spawn, execFile } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { query } from '@anthropic-ai/claude-agent-sdk';
import { listHistory } from './history.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const WORKER = path.join(HERE, 'worker.mjs');

// The wrapper that builds a sandbox. It sits beside the bridge once
// installed, and in the source tree while developing.
const SANDBOX_RUN = process.env.TG_SANDBOX_RUN ||
  path.join(path.dirname(HERE), 'tools', 'sandbox-run');
// Where the bridge's own code appears inside a sandbox, and so where the
// worker is started from in there.
const GUEST_BRIDGE = '/opt/bridge';

let pendingWork = 0; // async work that must finish before the process may exit

function out(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}

function childEnv() {
  return { ...process.env, IS_SANDBOX: '1' };
}

function agentOf(name) {
  switch (name) {
    case 'codex': return 'codex';
    case 'antigravity': case 'agy': return 'antigravity';
    default: return 'claude';
  }
}

// ---------------------------------------------------------------- workers

const workers = new Map(); // sid -> worker record
const configs = new Map(); // sid -> the last start command, replayed on respawn
const sandboxes = new Map(); // sid -> the folder an isolated session lives in

function worker(sid) {
  let w = workers.get(sid);
  if (w && w.proc && w.proc.exitCode === null && !w.proc.killed) return w;

  // An isolated session runs the whole worker inside the sandbox, so every
  // agent - and every command any of them runs - is behind the same boundary
  // rather than each having to be confined on its own terms.
  const dir = sandboxes.get(sid);
  const env = childEnv();
  let command = process.execPath;
  let argv = [WORKER, sid];
  if (dir) {
    env.SANDBOX_BRIDGE = HERE;
    // Bring the session's own services back up first: there is no init in
    // there, so a website it was running would otherwise be gone.
    env.SANDBOX_RESUME = '1';
    command = SANDBOX_RUN;
    argv = [dir, '/usr/local/bin/node', path.join(GUEST_BRIDGE, 'worker.mjs'), sid];
  }

  const proc = spawn(command, argv, {
    cwd: HERE,
    env,
    stdio: ['pipe', 'pipe', 'pipe'],
    // Its own process group: killing -pid takes the agent and everything it
    // started (bash, compilers, servers) with it.
    detached: true,
  });
  const rec = { proc, buf: '', running: false, last: '' };
  workers.set(sid, rec);

  // Decode as text: concatenating raw Buffers splits multi-byte characters
  // across chunk boundaries and turns them into replacement characters.
  proc.stdout.setEncoding('utf8');
  proc.stderr.setEncoding('utf8');

  // A worker that was just spawned starts from nothing, so the session's
  // settings are replayed into it before anything else.
  const saved = configs.get(sid);
  if (saved) {
    try { proc.stdin.write(JSON.stringify(saved) + '\n'); } catch { /* it will error below */ }
  }

  proc.stdout.on('data', chunk => {
    rec.buf += chunk;
    let i;
    while ((i = rec.buf.indexOf('\n')) >= 0) {
      const line = rec.buf.slice(0, i);
      rec.buf = rec.buf.slice(i + 1);
      if (!line.trim()) continue;
      let ev;
      try { ev = JSON.parse(line); } catch { continue; }
      if (ev.type === 'idle') rec.running = false;
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
    if (rec.quiet) return;
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
  // An isolated session names the folder its sandbox is built around. The
  // cwd it also carries is already the path as seen from inside, so nothing
  // below this point has to know the difference.
  if (msg.sandbox_dir) {
    if (sandboxes.get(sid) !== msg.sandbox_dir) {
      // A different folder means a different sandbox, so the worker inside
      // the old one cannot be reused.
      if (workers.has(sid)) discardWorker(sid);
      sandboxes.set(sid, msg.sandbox_dir);
    }
  } else if (msg.type === 'start') {
    if (sandboxes.has(sid)) discardWorker(sid);
    sandboxes.delete(sid);
  }
  // Remember the session's settings so a worker that was killed comes back
  // configured. The gateway sends the whole set with every prompt, so this is
  // only the safety net for a respawn.
  if (msg.type === 'start' || msg.type === 'prompt') {
    // Merge, never replace: a message that leaves a field out means "keep
    // what you had", so the remembered settings must not be thrown away by a
    // prompt that only carries the text.
    const saved = { ...(configs.get(sid) || {}), ...msg, type: 'start' };
    delete saved.text;
    delete saved.images;
    configs.set(sid, saved);
  }
  const w = worker(sid);
  if (msg.type === 'prompt') {
    // Count it as running from the moment it is sent: a turn that has not yet
    // reported back must not be treated as idle and torn down.
    w.running = true;
  }
  try {
    w.proc.stdin.write(JSON.stringify(msg) + '\n');
  } catch (err) {
    out({ type: 'error', sid, message: 'could not reach the agent process: ' + String(err?.message || err) });
  }
}

// discardWorker ends a worker quietly, because the session it belongs to has
// been reconfigured in a way it cannot follow - it moved in or out of a
// sandbox. Nothing is reported: the next prompt starts a fresh one.
function discardWorker(sid) {
  const w = workers.get(sid);
  workers.delete(sid);
  if (!w || !w.proc || w.proc.exitCode !== null) return;
  w.killedOnPurpose = true;
  w.quiet = true;
  try {
    process.kill(-w.proc.pid, 'SIGKILL');
  } catch {
    try { w.proc.kill('SIGKILL'); } catch { /* already gone */ }
  }
}

function killSession(sid, close) {
  const w = workers.get(sid);
  if (!w || !w.proc || w.proc.exitCode !== null) {
    if (close) {
      workers.delete(sid);
      configs.delete(sid);
    }
    out({ type: 'killed', sid, message: 'nothing was running' });
    return;
  }
  w.killedOnPurpose = true;
  if (close) configs.delete(sid);
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
  let models;
  if (agent === 'codex') models = codexModels();
  else if (agent === 'antigravity') models = await agyModels();
  else models = await claudeModels();
  modelCache.set(agent, { at: Date.now(), models });
  return models;
}

// Antigravity prints "id<tab>Label" from its own models command, so that is
// where its list comes from rather than anything hard-coded here.
function agyModels() {
  return new Promise(resolve => {
    execFile('agy', ['models'], { timeout: 60000, env: childEnv() }, (err, stdout) => {
      const efforts = ['low', 'medium', 'high'];
      const list = [{ id: '', label: 'Default', description: '', efforts, default_effort: '' }];
      for (const line of String(stdout || '').split('\n')) {
        const [id, label] = line.split('\t');
        if (!id || !label || id.includes(' ')) continue;
        list.push({ id: id.trim(), label: label.trim(), description: '', efforts, default_effort: '' });
      }
      resolve(list);
    });
  });
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

// ---------------------------------------------------------------- skills
//
// "Skills" means whatever the agent can be asked to run by name: Claude's
// skills, plugin skills and slash commands, and Codex's custom prompt files.

const skillCache = new Map();

async function listSkills(agent) {
  const hit = skillCache.get(agent);
  if (hit && Date.now() - hit.at < MODEL_TTL) return hit.skills;
  let skills;
  if (agent === 'codex') skills = codexPrompts();
  else if (agent === 'antigravity') skills = agySkills();
  else skills = await claudeSkills();
  skillCache.set(agent, { at: Date.now(), skills });
  return skills;
}

// Antigravity keeps skills as SKILL.md files, the same shape Claude uses, and
// expands /name in print mode, so listing the directories is enough.
function agySkills() {
  const home = os.homedir();
  const roots = [
    path.join(home, '.gemini', 'antigravity-cli', 'builtin', 'skills'),
    path.join(home, '.gemini', 'antigravity-cli', 'skills'),
    path.join(home, '.antigravity', 'skills'),
    path.join(home, '.agy', 'skills'),
  ];
  const seen = new Map();
  for (const root of roots) {
    let entries = [];
    try { entries = fs.readdirSync(root, { withFileTypes: true }); } catch { continue; }
    for (const e of entries) {
      if (!e.isDirectory()) continue;
      const file = path.join(root, e.name, 'SKILL.md');
      let body = '';
      try { body = fs.readFileSync(file, 'utf8'); } catch { continue; }
      const meta = skillFrontMatter(body);
      const name = meta.name || e.name;
      if (seen.has(name)) continue;
      seen.set(name, {
        name,
        description: (meta.description || promptDescription(body)).slice(0, 160),
        hint: '',
        skill: true,
      });
    }
  }
  return [...seen.values()].sort((a, b) => a.name.localeCompare(b.name));
}

function skillFrontMatter(body) {
  const fm = body.match(/^---\n([\s\S]*?)\n---/);
  if (!fm) return {};
  const out = {};
  for (const line of fm[1].split('\n')) {
    const m = line.match(/^(\w+):\s*(.+)$/);
    if (m) out[m[1]] = m[2].trim();
  }
  return out;
}

async function claudeSkills() {
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
      settingSources: ['user', 'project', 'local'],
      skills: 'all',
      abortController: ac,
      env: childEnv(),
    },
  });
  try {
    // Ask for the command list directly. Reading the message stream first
    // would block: nothing is emitted until a prompt is sent.
    const cmds = await q.supportedCommands();
    const fromDisk = skillNamesOnDisk();
    return cmds
      .filter(c => !HIDDEN_COMMANDS.has(c.name) && !c.name.startsWith('__'))
      .map(c => ({
        name: c.name,
        description: (c.description || '').split('\n')[0].slice(0, 160),
        hint: c.argumentHint || '',
        skill: fromDisk.has(c.name),
      }));
  } finally {
    release();
    try { await q.return?.(); } catch { /* already closed */ }
    ac.abort();
  }
}

// Skills the user installed, or that a plugin brought, live on disk as
// SKILL.md files. Knowing their names lets the gateway list them first.
function skillNamesOnDisk() {
  const home = os.homedir();
  const names = new Set();
  const roots = [
    path.join(home, '.claude', 'skills'),
    path.join(home, '.claude', 'plugins', 'marketplaces'),
  ];
  const walk = (dir, depth) => {
    if (depth > 5) return;
    let entries = [];
    try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
    for (const e of entries) {
      const full = path.join(dir, e.name);
      if (e.isDirectory()) {
        walk(full, depth + 1);
      } else if (e.name === 'SKILL.md') {
        const meta = skillFrontMatter(safeRead(full));
        names.add(meta.name || path.basename(dir));
      }
    }
  };
  for (const r of roots) walk(r, 0);
  return names;
}

function safeRead(file) {
  try { return fs.readFileSync(file, 'utf8'); } catch { return ''; }
}

// Commands that only make sense in a terminal, or that the gateway already
// offers as buttons of its own.
const HIDDEN_COMMANDS = new Set([
  'exit', 'quit', 'statusline', 'heapdump', 'color', 'vim', 'terminal-setup',
  'login', 'logout', 'upgrade', 'bug', 'release-notes', 'help',
  'model', 'clear', 'resume', 'fast', 'ide', 'install-github-app',
]);

// Codex has no skills, but it has prompt files, which are the same idea: a
// named instruction you can run. codex exec does not expand them, so the
// gateway reads them and sends the text itself.
function codexPrompts() {
  const dir = path.join(os.homedir(), '.codex', 'prompts');
  let names = [];
  try { names = fs.readdirSync(dir).filter(f => f.endsWith('.md')); } catch { return []; }
  return names.map(file => {
    const body = fs.readFileSync(path.join(dir, file), 'utf8');
    return {
      name: file.replace(/\.md$/, ''),
      description: promptDescription(body),
      hint: /\$ARGUMENTS|\$1/.test(body) ? '[arguments]' : '',
      skill: true,
      body,
    };
  });
}

function promptDescription(body) {
  const fm = body.match(/^---\n([\s\S]*?)\n---/);
  if (fm) {
    const d = fm[1].match(/^description:\s*(.+)$/m);
    if (d) return d[1].trim().slice(0, 160);
    body = body.slice(fm[0].length);
  }
  for (const line of body.split('\n')) {
    const t = line.replace(/^#+\s*/, '').trim();
    if (t) return t.slice(0, 160);
  }
  return '';
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
      listModels(agentOf(msg.agent))
        .then(models => out({ type: 'models', sid: msg.sid || '', agent: agentOf(msg.agent), models }))
        .catch(err => out({ type: 'error', sid: msg.sid || '', message: 'models: ' + String(err?.message || err) }))
        .finally(() => { pendingWork--; });
      break;
    }
    case 'skills': {
      pendingWork++;
      listSkills(agentOf(msg.agent))
        .then(skills => out({ type: 'skills', sid: msg.sid || '', agent: agentOf(msg.agent), skills }))
        .catch(err => out({ type: 'error', sid: msg.sid || '', message: 'skills: ' + String(err?.message || err) }))
        .finally(() => { pendingWork--; });
      break;
    }
    case 'history': {
      // Reading a few hundred files off disk, so it is done off the main path
      // and answered when it is ready.
      pendingWork++;
      const agent = agentOf(msg.agent);
      Promise.resolve()
        .then(() => listHistory(agent, msg.roots || [], msg.limit || 10))
        .then(sessions => out({ type: 'history', sid: msg.sid || '', agent, sessions }))
        .catch(err => out({ type: 'error', sid: msg.sid || '', message: 'history: ' + String(err?.message || err) }))
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
