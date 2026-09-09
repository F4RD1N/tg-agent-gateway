// Agent bridge.
//
// One Node process drives both agents through their official SDKs and speaks
// newline-delimited JSON with the Go gateway over stdin/stdout, so the Go side
// never has to parse a CLI's output format.
//
//   in :  {"type":"start","sid":"42","agent":"claude","cwd":"/root","model":"","resume":"<id>"}
//         {"type":"prompt","sid":"42","text":"hello"}
//         {"type":"interrupt","sid":"42"}  {"type":"stop","sid":"42"}  {"type":"ping"}
//   out:  started | delta | text | thinking | tool | file | todo | busy | done | error | pong
//
// Every out event carries the sid it belongs to. One turn runs at a time per
// sid; further prompts queue behind it.
import readline from 'node:readline';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { query } from '@anthropic-ai/claude-agent-sdk';
import { Codex } from '@openai/codex-sdk';

const sessions = new Map();
let codexClient = null;
let pendingWork = 0; // async work that must finish before the process may exit

function out(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}

function getCodex() {
  if (!codexClient) codexClient = new Codex();
  return codexClient;
}

function childEnv() {
  // Claude Code refuses to bypass permissions as root unless it believes it is
  // sandboxed. The gateway is deliberately full-access, so say so.
  return { ...process.env, IS_SANDBOX: '1' };
}

function session(sid) {
  let s = sessions.get(sid);
  if (!s) {
    s = {
      sid, agent: 'claude', cwd: process.cwd(), model: '', effort: '', ref: '',
      permMode: '', thinking: 0, maxTurns: 0, budget: 0, userSettings: false,
      sandbox: '', approval: '', webSearch: '', network: '',
      queue: [], running: false, abort: null,
    };
    sessions.set(sid, s);
  }
  return s;
}

// applyConfig copies whatever the Config button changed onto the session.
function applyConfig(s, msg) {
  s.permMode = msg.perm_mode || '';
  s.thinking = msg.thinking || 0;
  s.maxTurns = msg.max_turns || 0;
  s.budget = msg.budget_usd || 0;
  s.userSettings = !!msg.user_settings;
  s.sandbox = msg.sandbox || '';
  s.approval = msg.approval || '';
  s.webSearch = msg.web_search || '';
  s.network = msg.network || '';
}

function clip(s, n = 400) {
  if (typeof s !== 'string') s = String(s ?? '');
  s = s.replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n) + '…' : s;
}

// ---------------------------------------------------------------- claude

async function runClaude(s, text) {
  const ac = new AbortController();
  s.abort = () => ac.abort();
  const mode = s.permMode || 'bypassPermissions';
  const opts = {
    cwd: s.cwd,
    permissionMode: mode,
    allowDangerouslySkipPermissions: mode === 'bypassPermissions',
    // By default the user settings file is dropped: it carries this machine's
    // own permission rules, and the gateway decides its own policy. The
    // Config button can put it back.
    settingSources: s.userSettings ? ['user', 'project', 'local'] : [],
    includePartialMessages: true,
    abortController: ac,
    env: childEnv(),
  };
  if (s.model) opts.model = s.model;
  if (s.effort) opts.effort = s.effort;
  if (s.thinking > 0) opts.maxThinkingTokens = s.thinking;
  if (s.maxTurns > 0) opts.maxTurns = s.maxTurns;
  if (s.budget > 0) opts.maxBudgetUsd = s.budget;
  if (s.ref) opts.resume = s.ref;

  const toolNames = new Map();
  let sawText = false;

  const q = query({ prompt: text, options: opts });
  for await (const m of q) {
    switch (m.type) {
      case 'system':
        if (m.subtype === 'init' && m.session_id) {
          s.ref = m.session_id;
          out({ type: 'started', sid: s.sid, agent: 'claude', session: m.session_id, model: m.model || s.model });
        }
        break;
      case 'stream_event': {
        const ev = m.event;
        if (ev?.type === 'content_block_delta') {
          if (ev.delta?.type === 'text_delta' && ev.delta.text) {
            sawText = true;
            out({ type: 'delta', sid: s.sid, text: ev.delta.text });
          } else if (ev.delta?.type === 'thinking_delta' && ev.delta.thinking) {
            out({ type: 'thinking', sid: s.sid, text: ev.delta.thinking });
          }
        }
        break;
      }
      case 'assistant': {
        for (const c of m.message?.content || []) {
          if (c.type === 'text' && c.text && !sawText) {
            out({ type: 'text', sid: s.sid, text: c.text });
          } else if (c.type === 'tool_use') {
            toolNames.set(c.id, c.name);
            out({ type: 'tool', sid: s.sid, status: 'start', name: c.name, detail: describeClaudeTool(c.name, c.input) });
          }
        }
        sawText = false;
        break;
      }
      case 'user': {
        for (const c of m.message?.content || []) {
          if (c.type === 'tool_result') {
            const name = toolNames.get(c.tool_use_id) || 'tool';
            let body = '';
            if (typeof c.content === 'string') body = c.content;
            else if (Array.isArray(c.content)) body = c.content.filter(x => x.type === 'text').map(x => x.text).join('\n');
            out({
              type: 'tool', sid: s.sid, status: c.is_error ? 'fail' : 'ok',
              name, detail: clip(body, 700),
            });
          }
        }
        break;
      }
      case 'result': {
        if (m.session_id) s.ref = m.session_id;
        if (m.subtype !== 'success' && m.subtype !== 'error_max_turns' && m.result) {
          out({ type: 'error', sid: s.sid, message: clip(m.result, 800) });
        }
        out({
          type: 'done', sid: s.sid, session: s.ref,
          cost: m.total_cost_usd || 0,
          duration_ms: m.duration_ms || 0,
          turns: m.num_turns || 0,
          tokens: m.usage ? {
            input: (m.usage.input_tokens || 0) + (m.usage.cache_read_input_tokens || 0),
            output: m.usage.output_tokens || 0,
          } : null,
          subtype: m.subtype || '',
        });
        break;
      }
    }
  }
}

function describeClaudeTool(name, input) {
  if (!input) return '';
  switch (name) {
    case 'Bash': return clip(input.command || '', 300);
    case 'Read': case 'Write': case 'NotebookEdit': return clip(input.file_path || input.notebook_path || '', 300);
    case 'Edit': return clip(input.file_path || '', 300);
    case 'Glob': return clip(input.pattern || '', 200);
    case 'Grep': return clip((input.pattern || '') + (input.path ? ' in ' + input.path : ''), 300);
    case 'WebFetch': return clip(input.url || '', 200);
    case 'WebSearch': return clip(input.query || '', 200);
    case 'Task': return clip(input.description || '', 200);
    case 'TodoWrite': return (input.todos || []).length + ' items';
    default: return clip(JSON.stringify(input), 200);
  }
}

// ---------------------------------------------------------------- codex

async function runCodex(s, text) {
  const ac = new AbortController();
  s.abort = () => ac.abort();
  const threadOpts = {
    workingDirectory: s.cwd,
    sandboxMode: s.sandbox || 'danger-full-access',
    approvalPolicy: s.approval || 'never',
    skipGitRepoCheck: true,
  };
  if (s.model) threadOpts.model = s.model;
  if (s.effort) threadOpts.modelReasoningEffort = s.effort;
  if (s.webSearch) threadOpts.webSearchEnabled = s.webSearch === 'on';
  if (s.network) threadOpts.networkAccessEnabled = s.network === 'on';

  const codex = getCodex();
  const thread = s.ref ? codex.resumeThread(s.ref, threadOpts) : codex.startThread(threadOpts);
  const started = await thread.runStreamed(text, { signal: ac.signal });
  let usage = null;
  const t0 = Date.now();

  for await (const ev of started.events) {
    switch (ev.type) {
      case 'thread.started':
        s.ref = ev.thread_id;
        out({ type: 'started', sid: s.sid, agent: 'codex', session: ev.thread_id, model: s.model });
        break;
      case 'item.started':
      case 'item.updated':
      case 'item.completed': {
        const it = ev.item;
        const done = ev.type === 'item.completed';
        switch (it.type) {
          case 'agent_message':
            if (done && it.text) out({ type: 'text', sid: s.sid, text: it.text });
            break;
          case 'reasoning':
            if (done && it.text) out({ type: 'thinking', sid: s.sid, text: it.text });
            break;
          case 'command_execution':
            out({
              type: 'tool', sid: s.sid,
              status: it.status === 'in_progress' ? 'start' : (it.status === 'failed' || (it.exit_code ?? 0) !== 0 ? 'fail' : 'ok'),
              name: 'Bash',
              detail: it.status === 'in_progress' ? clip(it.command, 300) : clip(it.aggregated_output || '', 700),
            });
            break;
          case 'file_change':
            if (done) {
              for (const ch of it.changes || []) {
                out({ type: 'file', sid: s.sid, path: ch.path, kind: ch.kind, ok: it.status === 'completed' });
              }
            }
            break;
          case 'mcp_tool_call':
            out({
              type: 'tool', sid: s.sid,
              status: it.status === 'in_progress' ? 'start' : (it.status === 'failed' ? 'fail' : 'ok'),
              name: `${it.server}/${it.tool}`,
              detail: it.error ? clip(it.error.message, 300) : clip(JSON.stringify(it.arguments || {}), 300),
            });
            break;
          case 'web_search':
            if (done) out({ type: 'tool', sid: s.sid, status: 'ok', name: 'WebSearch', detail: clip(it.query, 200) });
            break;
          case 'todo_list':
            if (done) out({ type: 'todo', sid: s.sid, items: (it.items || []).map(i => ({ text: i.text, done: !!i.completed })) });
            break;
          case 'error':
            out({ type: 'error', sid: s.sid, message: clip(it.message, 800) });
            break;
        }
        break;
      }
      case 'turn.completed':
        usage = ev.usage || null;
        break;
      case 'turn.failed':
        out({ type: 'error', sid: s.sid, message: clip(ev.error?.message || 'turn failed', 800) });
        break;
      case 'error':
        out({ type: 'error', sid: s.sid, message: clip(ev.message || 'error', 800) });
        break;
    }
  }
  out({
    type: 'done', sid: s.sid, session: s.ref,
    cost: 0,
    duration_ms: Date.now() - t0,
    tokens: usage ? { input: (usage.input_tokens || 0), output: (usage.output_tokens || 0) } : null,
    subtype: 'success',
  });
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
// a query with an iterator that stays open, asks, and shuts it down again. No
// prompt is ever sent, so the turn costs nothing.
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
      label: m.displayName || m.value,
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

function codexModels() {
  const file = path.join(os.homedir(), '.codex', 'models_cache.json');
  let raw = null;
  try { raw = JSON.parse(fs.readFileSync(file, 'utf8')); } catch { /* not cached yet */ }
  const list = (raw?.models || [])
    .filter(m => m && m.slug && m.visibility !== 'hidden')
    .map(m => ({
      id: m.slug,
      label: m.display_name || m.slug,
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

// ---------------------------------------------------------------- queue

async function pump(s) {
  if (s.running) return;
  s.running = true;
  while (s.queue.length) {
    const text = s.queue.shift();
    try {
      if (s.agent === 'codex') await runCodex(s, text);
      else await runClaude(s, text);
    } catch (err) {
      const msg = String(err?.message || err);
      const aborted = /abort/i.test(msg);
      out({ type: aborted ? 'stopped' : 'error', sid: s.sid, message: aborted ? 'stopped' : clip(msg, 800) });
      if (!aborted) out({ type: 'done', sid: s.sid, session: s.ref, cost: 0, duration_ms: 0, subtype: 'error' });
      else out({ type: 'done', sid: s.sid, session: s.ref, cost: 0, duration_ms: 0, subtype: 'stopped' });
    } finally {
      s.abort = null;
    }
  }
  s.running = false;
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
// stdin closing means the gateway is going away; finish what is already
// running (a turn costs money and may be half-way through an edit) and exit.
rl.on('close', () => {
  const wait = () => {
    const busy = pendingWork > 0 || [...sessions.values()].some(s => s.running);
    if (busy) setTimeout(wait, 250);
    else process.exit(0);
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
    case 'start': {
      const s = session(msg.sid);
      s.agent = msg.agent === 'codex' ? 'codex' : 'claude';
      if (msg.cwd) s.cwd = msg.cwd;
      s.model = msg.model || '';
      s.effort = msg.effort || '';
      s.ref = msg.resume || '';
      applyConfig(s, msg);
      out({ type: 'ok', sid: s.sid, agent: s.agent, session: s.ref });
      break;
    }
    case 'prompt': {
      const s = session(msg.sid);
      if (msg.cwd) s.cwd = msg.cwd;
      if (msg.agent) s.agent = msg.agent === 'codex' ? 'codex' : 'claude';
      if (msg.model !== undefined) s.model = msg.model || '';
      if (msg.effort !== undefined) s.effort = msg.effort || '';
      if (msg.resume !== undefined) s.ref = msg.resume || '';
      applyConfig(s, msg);
      s.queue.push(String(msg.text || ''));
      if (s.running) out({ type: 'busy', sid: s.sid, queued: s.queue.length });
      pump(s);
      break;
    }
    case 'interrupt': {
      const s = sessions.get(msg.sid);
      if (s && s.abort) s.abort();
      else out({ type: 'stopped', sid: msg.sid || '', message: 'nothing running' });
      break;
    }
    case 'clear': {
      const s = session(msg.sid);
      s.ref = '';
      s.queue.length = 0;
      out({ type: 'ok', sid: s.sid, session: '' });
      break;
    }
    case 'stop': {
      const s = sessions.get(msg.sid);
      if (s) {
        if (s.abort) s.abort();
        sessions.delete(msg.sid);
      }
      out({ type: 'ok', sid: msg.sid || '' });
      break;
    }
  }
}

process.on('uncaughtException', err => {
  out({ type: 'error', sid: '', message: 'bridge: ' + String(err?.message || err) });
});
process.on('unhandledRejection', err => {
  out({ type: 'error', sid: '', message: 'bridge: ' + String(err?.message || err) });
});

out({ type: 'hello', pid: process.pid, node: process.version });
