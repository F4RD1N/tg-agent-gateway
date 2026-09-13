// Agent worker: one process per session.
//
// Running each session in its own process means the gateway can kill a
// runaway agent - and every command it spawned - by killing this process
// group, without touching the other sessions. It speaks the same JSONL as
// the bridge does, on stdin and stdout.
import readline from 'node:readline';
import { spawn } from 'node:child_process';
import { query } from '@anthropic-ai/claude-agent-sdk';
import { Codex } from '@openai/codex-sdk';
import { runCodexStream } from './codex-turn.mjs';

const SID = process.argv[2] || '';

// Ultracode: the effort level that means "put a crowd of agents on it".
const ULTRACODE = 'ultracode';
const ULTRACODE_ASK =
  '[ultracode] I am explicitly asking for multi-agent orchestration on this: ' +
  'use the Workflow tool to fan the work out across agents and verify the ' +
  'results, rather than doing it all yourself, whenever the job is big enough ' +
  'to be worth it. Say so plainly if it is not.\n\n';
let codexClient = null;

function out(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}

function getCodex() {
  // The SDK stops inheriting process.env once env is supplied, so the full
  // environment is passed through with the topic destination added.
  const env = childEnv();
  // Codex's speed setting is a config key rather than a thread option, so it
  // belongs to the client and a change to it needs a new one.
  const config = {};
  // "fast" is the priority lane the models advertise; "default" is the
  // ordinary one. Codex warns and ignores anything else, so these two are the
  // only values worth sending.
  if (s.fast === 'on') config.service_tier = 'fast';
  else if (s.fast === 'off') config.service_tier = 'default';
  const key = JSON.stringify([env.AGENT_TG_CHAT_ID, env.AGENT_TG_TOPIC_ID, s.fast || '']);
  if (!codexClient || codexClient.__key !== key) {
    codexClient = new Codex({ env, config });
    codexClient.__key = key;
  }
  return codexClient;
}

function childEnv() {
  // Claude Code refuses to bypass permissions as root unless it believes it is
  // sandboxed. The gateway is deliberately full-access, so say so.
  const env = { ...process.env, IS_SANDBOX: '1' };
  // Where this session's files belong: its own Telegram topic. tg-send and the
  // older delivery helpers read these, so anything the agent delivers arrives
  // in the topic the work is happening in.
  if (s.tgChat) {
    env.AGENT_TG_CHAT_ID = String(s.tgChat);
    env.AGENT_TG_TOPIC_ID = String(s.tgTopic || '');
    env.AGENT_TG_TOKEN = s.tgToken || '';
    env.AGENT_SESSION_TOPIC = s.tgTitle || '';
  }
  return env;
}

const s = {
  sid: SID, agent: 'claude', cwd: process.cwd(), model: '', effort: '', ref: '',
  permMode: '', thinking: 0, maxTurns: 0, budget: 0, noUserSettings: false, fallback: '',
  sandbox: '', approval: '', webSearch: '', network: '', fast: '',
  tgChat: '', tgTopic: '', tgToken: '', tgTitle: '',
  queue: [], running: false, abort: null,
};

// applyConfig copies whatever the Config button changed onto the session.
function applyConfig(s, msg) {
  s.permMode = msg.perm_mode || '';
  s.thinking = msg.thinking || 0;
  s.maxTurns = msg.max_turns || 0;
  s.budget = msg.budget_usd || 0;
  s.noUserSettings = !!msg.no_user_settings;
  s.fallback = msg.fallback_model || '';
  if (msg.tg_chat !== undefined) s.tgChat = msg.tg_chat || '';
  if (msg.tg_topic !== undefined) s.tgTopic = msg.tg_topic || '';
  if (msg.tg_token !== undefined) s.tgToken = msg.tg_token || '';
  if (msg.tg_title !== undefined) s.tgTitle = msg.tg_title || '';
  s.sandbox = msg.sandbox || '';
  s.approval = msg.approval || '';
  s.fast = msg.fast || '';
  s.webSearch = msg.web_search || '';
  s.network = msg.network || '';
}

function normaliseAgent(name) {
  switch (name) {
    case 'codex': return 'codex';
    case 'antigravity': case 'agy': return 'antigravity';
    default: return 'claude';
  }
}

function clip(str, n = 400) {
  if (typeof str !== 'string') str = String(str ?? '');
  str = str.replace(/\s+/g, ' ').trim();
  return str.length > n ? str.slice(0, n) + '…' : str;
}

// ---------------------------------------------------------------- claude

async function runClaude(s, text, images) {
  const ac = new AbortController();
  s.abort = () => ac.abort();
  const mode = s.permMode || 'bypassPermissions';
  const opts = {
    cwd: s.cwd,
    permissionMode: mode,
    allowDangerouslySkipPermissions: mode === 'bypassPermissions',
    // Load the machine's own settings by default: that is where the user's
    // skills, plugins and CLAUDE.md live, and this gateway is meant to feel
    // like their own Claude Code. Config can turn it off per session.
    settingSources: s.noUserSettings ? [] : ['user', 'project', 'local'],
    // Every skill the CLI can find, including ones plugins bring.
    skills: 'all',
    includePartialMessages: true,
    abortController: ac,
    env: childEnv(),
  };
  if (s.model) opts.model = s.model;
  if (s.effort) opts.effort = s.effort === ULTRACODE ? 'xhigh' : s.effort;
  if (s.thinking > 0) opts.maxThinkingTokens = s.thinking;
  if (s.maxTurns > 0) opts.maxTurns = s.maxTurns;
  if (s.budget > 0) opts.maxBudgetUsd = s.budget;
  // "Switch models when a message is flagged": retry a refused turn here.
  if (s.fallback) opts.fallbackModel = s.fallback;
  if (s.ref) opts.resume = s.ref;

  const toolNames = new Map();
  let sawText = false;

  let prompt = text;
  // Ultracode is xhigh thinking plus multi-agent orchestration. The effort
  // name alone does not switch orchestration on through the SDK - the
  // Workflow tool only acts when the person asks for one - so the asking is
  // done here, in the words the tool is waiting for.
  if (s.effort === ULTRACODE && typeof prompt === 'string' && !prompt.startsWith(ULTRACODE_ASK)) {
    prompt = ULTRACODE_ASK + prompt;
  }
  if (images && images.length) {
    // Claude Code views images with its Read tool; saying so plainly stops it
    // trying to cat the file first.
    const list = images.join('\n');
    prompt = text + '\n\nAttached image' + (images.length > 1 ? 's' : '') + ':\n' + list +
      '\nOpen ' + (images.length > 1 ? 'them' : 'it') + ' with the Read tool (not Bash) before answering.';
  }
  const q = query({ prompt, options: opts });
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

// ---------------------------------------------------------------- antigravity
//
// Antigravity ships a CLI rather than an SDK, but it speaks the same shape:
// one NDJSON event per line, a conversation id to resume by, and permission
// flags. So it is driven directly.

function runAntigravity(s, text, images) {
  return new Promise((resolve, reject) => {
    const args = ['--output-format', 'stream-json'];
    switch (s.permMode) {
      case 'plan':
        args.push('--mode', 'plan');
        break;
      case 'acceptEdits':
        args.push('--mode', 'accept-edits');
        break;
      default:
        args.push('--dangerously-skip-permissions');
    }
    if (s.model) args.push('--model', s.model);
    if (s.effort) args.push('--effort', s.effort);
    if (s.ref) args.push('--conversation', s.ref);
    let prompt = text;
    if (images && images.length) {
      prompt += '\n\nAttached image' + (images.length > 1 ? 's' : '') + ':\n' + images.join('\n') +
        '\nOpen ' + (images.length > 1 ? 'them' : 'it') + ' before answering.';
    }
    args.push('--print=' + prompt);

    const child = spawn('agy', args, {
      cwd: s.cwd, env: childEnv(), stdio: ['ignore', 'pipe', 'pipe'],
      detached: true, // its own group, so an interrupt reaches what it spawned
    });
    child.stdout.setEncoding('utf8');
    child.stderr.setEncoding('utf8');
    let hardKill = null;
    s.abort = () => {
      try { process.kill(-child.pid, 'SIGTERM'); } catch { try { child.kill('SIGTERM'); } catch { /* gone */ } }
      // If it ignores the polite signal, end it, or the turn never settles.
      hardKill = setTimeout(() => {
        try { process.kill(-child.pid, 'SIGKILL'); } catch { try { child.kill('SIGKILL'); } catch { /* gone */ } }
      }, 5000);
    };

    let buf = '';
    let stderr = '';
    const t0 = Date.now();
    const tools = new Map(); // step_index -> tool name, so a start is only announced once

    child.stdout.on('data', chunk => {
      buf += chunk;
      let i;
      while ((i = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, i).trim();
        buf = buf.slice(i + 1);
        if (!line.startsWith('{')) continue;
        let ev;
        try { ev = JSON.parse(line); } catch { continue; }
        handleAgyEvent(s, ev, tools, t0);
      }
    });
    child.stderr.on('data', d => { stderr += d; if (stderr.length > 4000) stderr = stderr.slice(-4000); });

    child.on('error', err => {
      if (err && err.code === 'ENOENT') {
        reject(new Error('Antigravity is not installed on this server (the agy command was not found)'));
        return;
      }
      reject(err);
    });
    child.on('exit', (code, signal) => {
      s.abort = null;
      if (hardKill) clearTimeout(hardKill);
      const finished = s.agyFinished;
      s.agyFinished = false;
      if (signal) {
        reject(new Error('aborted'));
        return;
      }
      if (code !== 0 && !finished) {
        out({ type: 'error', sid: s.sid, message: clip(stderr || `agy exited with ${code}`, 800) });
        out({ type: 'done', sid: s.sid, session: s.ref, cost: 0, duration_ms: Date.now() - t0, subtype: 'error' });
      }
      resolve();
    });
  });
}

function handleAgyEvent(s, ev, tools, t0) {
  switch (ev.event) {
    case 'init':
      if (ev.conversation_id) {
        s.ref = ev.conversation_id;
        out({ type: 'started', sid: s.sid, agent: 'antigravity', session: s.ref, model: s.model });
      }
      break;
    case 'step_update': {
      const su = ev.step_update || {};
      if (su.conversation_id && !s.ref) s.ref = su.conversation_id;
      if (su.step_type === 'agent_response' && su.text_delta) {
        out({ type: 'delta', sid: s.sid, text: su.text_delta });
        break;
      }
      if (su.step_type === 'tool') {
        const info = su.tool_info || {};
        const name = su.tool_name || info.name || 'tool';
        if (su.state === 'ACTIVE' && !tools.has(su.step_index)) {
          tools.set(su.step_index, name);
          out({ type: 'tool', sid: s.sid, status: 'start', name: agyToolName(name), detail: agyToolDetail(name, info) });
        } else if (su.state === 'DONE') {
          const failed = /error|fail/i.test(String(info.status || ''));
          out({
            type: 'tool', sid: s.sid, status: failed ? 'fail' : 'ok',
            name: agyToolName(name), detail: clip(String(info.output || ''), 700),
          });
        }
      }
      break;
    }
    case 'result': {
      const r = ev.result || {};
      s.agyFinished = true;
      if (r.conversation_id) s.ref = r.conversation_id;
      if (r.status && r.status !== 'SUCCESS') {
        out({ type: 'error', sid: s.sid, message: clip(r.error || r.status, 800) });
      }
      out({
        type: 'done', sid: s.sid, session: s.ref,
        cost: 0,
        duration_ms: Math.round((r.duration_seconds || (Date.now() - t0) / 1000) * 1000),
        turns: r.num_turns || 0,
        tokens: r.usage ? { input: r.usage.input_tokens || 0, output: r.usage.output_tokens || 0 } : null,
        subtype: r.status === 'SUCCESS' ? 'success' : 'error',
      });
      break;
    }
  }
}

// Antigravity's tool names map onto the same handful of ideas the other
// agents use, so the step lines read the same way.
function agyToolName(name) {
  switch (name) {
    case 'run_command': return 'Bash';
    case 'view_file': case 'read_file': return 'Read';
    case 'write_to_file': case 'create_file': return 'Write';
    case 'replace_file_content': case 'edit_file': return 'Edit';
    case 'grep_search': case 'codebase_search': return 'Grep';
    case 'find_by_name': case 'list_dir': return 'Glob';
    case 'read_url_content': case 'read_web_page': return 'WebFetch';
    case 'search_web': return 'WebSearch';
    default:
      if (name.startsWith('browser_')) return 'Browser';
      return name;
  }
}

function agyToolDetail(name, info) {
  const p = info.parameters || {};
  if (name === 'run_command') return clip(p.CommandLine || p.command || '', 300);
  for (const key of ['AbsolutePath', 'TargetFile', 'File', 'path', 'Query', 'SearchTerm', 'Url', 'query']) {
    if (p[key]) return clip(String(p[key]), 300);
  }
  return clip(JSON.stringify(p), 200);
}

// ---------------------------------------------------------------- codex

async function runCodex(s, text, images) {
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
  // Codex takes images as input directly, which is far better than telling it
  // where a file is and hoping it opens it.
  let input = text;
  if (images && images.length) {
    input = [{ type: 'text', text }, ...images.map(path => ({ type: 'local_image', path }))];
  }
  const t0 = Date.now();
  const { usage } = await runCodexStream({
    thread, input, signal: ac.signal,
    onNotice: message => out({ type: 'note', sid: s.sid, message }),
    onEvent: ev => {
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
          }
          break;
        }
      }
    },
  });
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
    const { text, images } = s.queue.shift();
    try {
      if (s.agent === 'codex') await runCodex(s, text, images);
      else if (s.agent === 'antigravity') await runAntigravity(s, text, images);
      else await runClaude(s, text, images);
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
        s.agent = normaliseAgent(msg.agent);
        if (msg.cwd) s.cwd = msg.cwd;
        s.model = msg.model || '';
        s.effort = msg.effort || '';
        s.ref = msg.resume || '';
        applyConfig(s, msg);
        out({ type: 'ok', sid: s.sid, agent: s.agent, session: s.ref });
        break;
      case 'prompt':
        if (msg.cwd) s.cwd = msg.cwd;
        if (msg.agent) s.agent = normaliseAgent(msg.agent);
        if (msg.model !== undefined) s.model = msg.model || '';
        if (msg.effort !== undefined) s.effort = msg.effort || '';
        if (msg.resume !== undefined) s.ref = msg.resume || '';
        applyConfig(s, msg);
        s.queue.push({ text: String(msg.text || ''), images: Array.isArray(msg.images) ? msg.images : [] });
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
