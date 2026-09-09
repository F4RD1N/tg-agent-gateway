// Agent worker: one process per session.
//
// Running each session in its own process means the gateway can kill a
// runaway agent - and every command it spawned - by killing this process
// group, without touching the other sessions. It speaks the same JSONL as
// the bridge does, on stdin and stdout.
import readline from 'node:readline';
import { query } from '@anthropic-ai/claude-agent-sdk';
import { Codex } from '@openai/codex-sdk';

const SID = process.argv[2] || '';
let codexClient = null;

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

const s = {
  sid: SID, agent: 'claude', cwd: process.cwd(), model: '', effort: '', ref: '',
  permMode: '', thinking: 0, maxTurns: 0, budget: 0, userSettings: false, fallback: '',
  sandbox: '', approval: '', webSearch: '', network: '',
  queue: [], running: false, abort: null,
};

// applyConfig copies whatever the Config button changed onto the session.
function applyConfig(s, msg) {
  s.permMode = msg.perm_mode || '';
  s.thinking = msg.thinking || 0;
  s.maxTurns = msg.max_turns || 0;
  s.budget = msg.budget_usd || 0;
  s.userSettings = !!msg.user_settings;
  s.fallback = msg.fallback_model || '';
  s.sandbox = msg.sandbox || '';
  s.approval = msg.approval || '';
  s.webSearch = msg.web_search || '';
  s.network = msg.network || '';
}

function clip(str, n = 400) {
  if (typeof str !== 'string') str = String(str ?? '');
  str = str.replace(/\s+/g, ' ').trim();
  return str.length > n ? str.slice(0, n) + '…' : str;
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
  // "Switch models when a message is flagged": retry a refused turn here.
  if (s.fallback) opts.fallbackModel = s.fallback;
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
      case 'compact_boundary': {
        const pre = m.compact_metadata?.pre_tokens;
        out({
          type: 'note', sid: s.sid,
          message: 'context compacted' + (pre ? ` (was ${Math.round(pre / 1000)}k tokens)` : ''),
        });
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

// ---------------------------------------------------------------- queue

async function pump() {
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
      out({ type: 'done', sid: s.sid, session: s.ref, cost: 0, duration_ms: 0, subtype: aborted ? 'stopped' : 'error' });
    } finally {
      s.abort = null;
    }
  }
  s.running = false;
  out({ type: 'idle', sid: s.sid });
}

const rl = readline.createInterface({ input: process.stdin });
rl.on('line', line => {
  line = line.trim();
  if (!line) return;
  let msg;
  try { msg = JSON.parse(line); } catch { return; }
  try {
    switch (msg.type) {
      case 'start':
        s.agent = msg.agent === 'codex' ? 'codex' : 'claude';
        if (msg.cwd) s.cwd = msg.cwd;
        s.model = msg.model || '';
        s.effort = msg.effort || '';
        s.ref = msg.resume || '';
        applyConfig(s, msg);
        out({ type: 'ok', sid: s.sid, agent: s.agent, session: s.ref });
        break;
      case 'prompt':
        if (msg.cwd) s.cwd = msg.cwd;
        if (msg.agent) s.agent = msg.agent === 'codex' ? 'codex' : 'claude';
        if (msg.model !== undefined) s.model = msg.model || '';
        if (msg.effort !== undefined) s.effort = msg.effort || '';
        if (msg.resume !== undefined) s.ref = msg.resume || '';
        applyConfig(s, msg);
        s.queue.push(String(msg.text || ''));
        if (s.running) out({ type: 'busy', sid: s.sid, queued: s.queue.length });
        pump();
        break;
      case 'interrupt':
        if (s.abort) s.abort();
        else out({ type: 'stopped', sid: s.sid, message: 'nothing running' });
        break;
      case 'clear':
        s.ref = '';
        s.queue.length = 0;
        out({ type: 'ok', sid: s.sid, session: '' });
        break;
    }
  } catch (err) {
    out({ type: 'error', sid: s.sid, message: String(err?.message || err) });
  }
});
rl.on('close', () => {
  const wait = () => (s.running ? setTimeout(wait, 200) : process.exit(0));
  wait();
});

process.on('uncaughtException', err => out({ type: 'error', sid: SID, message: 'worker: ' + String(err?.message || err) }));
process.on('unhandledRejection', err => out({ type: 'error', sid: SID, message: 'worker: ' + String(err?.message || err) }));
