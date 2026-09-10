// Past conversations, read from where each agent keeps them.
//
// This is what "/sessions" browses: the agent's own history, not the
// gateway's, so a conversation started before the bot existed - or in a topic
// that has since been deleted - can still be found and resumed. Each agent
// stores it differently: Claude writes one JSONL file per session, Codex
// writes a rollout file per day, Antigravity keeps a small SQLite database
// per conversation.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createRequire } from 'node:module';

// SQLite is still flagged experimental in Node, and the warning would land in
// the gateway's log every time somebody opened the menu.
process.removeAllListeners('warning');

const HEAD = 256 * 1024; // enough of a transcript to find its first message

function head(file, bytes = HEAD) {
  let fd;
  try {
    fd = fs.openSync(file, 'r');
    const buf = Buffer.alloc(bytes);
    const n = fs.readSync(fd, buf, 0, bytes, 0);
    return buf.subarray(0, n).toString('utf8');
  } catch {
    return '';
  } finally {
    if (fd !== undefined) try { fs.closeSync(fd); } catch { /* already closed */ }
  }
}

function lines(text) {
  const out = text.split('\n');
  // The last line of a truncated read is usually half a record.
  out.pop();
  return out;
}

function clip(s, n = 160) {
  s = String(s ?? '').replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n) + '…' : s;
}

// strip removes what the gateway and the tools put in front of a prompt, so
// the preview is the person's own words rather than the standing instructions
// every turn carries.
function strip(text) {
  let t = String(text || '');
  t = t.replace(/<environment_context>[\s\S]*?<\/environment_context>/g, '');
  t = t.replace(/<user_instructions>[\s\S]*?<\/user_instructions>/g, '');
  t = t.replace(/<[a-z_]*SKILL[a-z_]*>[\s\S]*?(<\/[a-z_]*SKILL[a-z_]*>|$)/gi, '');
  // The gateway's own preamble, with or without its opening bracket, which
  // Antigravity's storage clips.
  const preamble = /\[?gateway\]\s*You are answering inside a Telegram topic\.[\s\S]*?on a phone\.\s*/i;
  t = t.replace(preamble, '');
  return t.trim();
}

// Prompts the gateway or the tools generated, which say nothing about what
// the conversation was for.
function boilerplate(text) {
  const t = String(text || '').trim();
  if (!t) return true;
  if (t.startsWith('<')) return true; // <command-name>, <local-command-caveat>, …
  if (t.startsWith('Caveat:')) return true;
  // A skill's own instructions, which Antigravity stores ahead of the prompt.
  if (/^[A-Za-z_]{2,20}>/.test(t)) return true;
  if (/The user has explicitly invoked/.test(t)) return true;
  return false;
}

function statOf(file) {
  try { return fs.statSync(file); } catch { return null; }
}

function listDir(dir) {
  try { return fs.readdirSync(dir, { withFileTypes: true }); } catch { return []; }
}

// homes lists the places to look: this machine's own home, and the private
// home inside each isolated session, so a sandboxed conversation is listed
// too - under its own folder, never mixed in with anybody else's.
function homes(roots) {
  const out = [{ home: os.homedir(), dir: '', isolated: false }];
  for (const root of roots || []) {
    for (const agentDir of listDir(root)) {
      if (!agentDir.isDirectory()) continue;
      const agentPath = path.join(root, agentDir.name);
      for (const sessDir of listDir(agentPath)) {
        if (!sessDir.isDirectory()) continue;
        const dir = path.join(agentPath, sessDir.name);
        const home = path.join(dir, '.home');
        if (statOf(home)) out.push({ home, dir, isolated: true });
      }
    }
  }
  return out;
}

// ---------------------------------------------------------------- claude

function claudeHistory(roots, limit) {
  const found = [];
  for (const place of homes(roots)) {
    const base = path.join(place.home, '.claude', 'projects');
    for (const project of listDir(base)) {
      if (!project.isDirectory()) continue;
      for (const f of listDir(path.join(base, project.name))) {
        if (!f.isFile() || !f.name.endsWith('.jsonl')) continue;
        const file = path.join(base, project.name, f.name);
        const st = statOf(file);
        if (!st || st.size === 0) continue;
        found.push({ file, when: st.mtime, place });
      }
    }
  }
  found.sort((a, b) => b.when - a.when);

  const out = [];
  for (const hit of found) {
    if (out.length >= limit) break;
    const s = claudeSession(hit);
    if (s) out.push(s);
  }
  return out;
}

function claudeSession(hit) {
  const id = path.basename(hit.file, '.jsonl');
  let cwd = '';
  let preview = '';
  let messages = 0;
  for (const line of lines(head(hit.file))) {
    if (!line.trim()) continue;
    let rec;
    try { rec = JSON.parse(line); } catch { continue; }
    if (!cwd && rec.cwd) cwd = rec.cwd;
    if (rec.type === 'user' || rec.type === 'assistant') messages++;
    if (preview || rec.type !== 'user' || rec.isMeta || rec.isSidechain) continue;
    const content = rec.message?.content;
    const text = typeof content === 'string'
      ? content
      : Array.isArray(content) ? content.filter(c => c?.type === 'text').map(c => c.text).join(' ') : '';
    const cleaned = strip(text);
    if (!boilerplate(cleaned)) preview = clip(cleaned);
  }
  if (!preview && !messages) return null;
  return {
    id,
    preview: preview || '(no message)',
    cwd: hit.place.isolated ? hit.place.dir : cwd,
    when: hit.when.toISOString(),
    messages,
    isolated: hit.place.isolated,
  };
}

// ---------------------------------------------------------------- codex

function codexHistory(roots, limit) {
  const found = [];
  for (const place of homes(roots)) {
    const base = path.join(place.home, '.codex', 'sessions');
    // year / month / day, newest first, and stop once there is plenty.
    for (const year of descend(base)) {
      for (const month of descend(year)) {
        for (const day of descend(month)) {
          for (const f of listDir(day)) {
            if (!f.isFile() || !f.name.endsWith('.jsonl')) continue;
            const file = path.join(day, f.name);
            const st = statOf(file);
            if (!st || st.size === 0) continue;
            found.push({ file, when: st.mtime, place });
          }
        }
      }
    }
  }
  found.sort((a, b) => b.when - a.when);

  // A conversation that was resumed has a rollout file per run under the same
  // id; only the most recent one is worth listing.
  const out = [];
  const seen = new Set();
  for (const hit of found) {
    if (out.length >= limit) break;
    const s = codexSession(hit);
    if (!s || seen.has(s.id)) continue;
    seen.add(s.id);
    out.push(s);
  }
  return out;
}

// descend lists a directory's subdirectories, newest name first, which for
// Codex's year/month/day layout is newest first.
function descend(dir) {
  return listDir(dir)
    .filter(e => e.isDirectory())
    .map(e => e.name)
    .sort((a, b) => b.localeCompare(a))
    .map(name => path.join(dir, name));
}

function codexSession(hit) {
  let id = '';
  let cwd = '';
  let preview = '';
  let messages = 0;
  for (const line of lines(head(hit.file))) {
    if (!line.trim()) continue;
    let rec;
    try { rec = JSON.parse(line); } catch { continue; }
    const p = rec.payload || {};
    if (rec.type === 'session_meta') {
      id = p.session_id || p.id || id;
      cwd = p.cwd || cwd;
      continue;
    }
    if (rec.type !== 'response_item' || p.type !== 'message') continue;
    if (p.role === 'assistant') messages++;
    if (p.role !== 'user') continue;
    messages++;
    if (preview) continue;
    const text = (p.content || [])
      .filter(c => c?.type === 'input_text' || c?.type === 'text')
      .map(c => c.text).join(' ');
    // Codex's first user turn carries the environment preamble.
    const cleaned = strip(text);
    if (!boilerplate(cleaned)) preview = clip(cleaned);
  }
  if (!id) {
    const m = path.basename(hit.file).match(/rollout-.*?-([0-9a-f-]{36})\.jsonl$/i);
    id = m ? m[1] : '';
  }
  if (!id) return null;
  return {
    id,
    preview: preview || '(no message)',
    cwd: hit.place.isolated ? hit.place.dir : cwd,
    when: hit.when.toISOString(),
    messages,
    isolated: hit.place.isolated,
  };
}

// ------------------------------------------------------------ antigravity

function agyHistory(roots, limit) {
  const found = [];
  for (const place of homes(roots)) {
    const base = path.join(place.home, '.gemini', 'antigravity-cli', 'conversations');
    for (const f of listDir(base)) {
      if (!f.isFile() || !f.name.endsWith('.db')) continue;
      const file = path.join(base, f.name);
      const st = statOf(file);
      if (!st || st.size === 0) continue;
      found.push({ file, when: st.mtime, place });
    }
  }
  found.sort((a, b) => b.when - a.when);

  const summaries = agySummaries();
  const out = [];
  for (const hit of found) {
    if (out.length >= limit) break;
    const s = agySession(hit, summaries);
    if (s) out.push(s);
  }
  return out;
}

const load = createRequire(import.meta.url);

function openDB(file) {
  try {
    // Loaded on demand rather than imported, so an older Node without the
    // SQLite module still runs everything else in here.
    const { DatabaseSync } = load('node:sqlite');
    return new DatabaseSync(file, { readOnly: true });
  } catch {
    return null;
  }
}

// The summary database has a title and preview for some conversations; it is
// often empty, so it is a bonus rather than the source.
function agySummaries() {
  const file = path.join(os.homedir(), '.gemini', 'antigravity-cli', 'conversation_summaries.db');
  const out = new Map();
  if (!statOf(file)) return out;
  const db = openDB(file);
  if (!db) return out;
  try {
    for (const row of db.prepare('select conversation_id, title, preview, step_count from conversation_summaries').all()) {
      out.set(row.conversation_id, row);
    }
  } catch { /* an older schema; the previews below still work */ } finally {
    try { db.close(); } catch { /* already closed */ }
  }
  return out;
}

function agySession(hit, summaries) {
  const id = path.basename(hit.file, '.db');
  const known = summaries.get(id);
  let preview = clip(known?.title || known?.preview || '');
  let messages = known?.step_count || 0;
  const db = openDB(hit.file);
  if (db) {
    try {
      if (!messages) {
        messages = db.prepare('select count(*) as n from steps').get()?.n || 0;
      }
      if (!preview) {
        // The first steps are the standing instructions; the person's own
        // words are in whichever of the early ones is not boilerplate.
        for (const row of db.prepare('select step_payload from steps order by idx limit 4').all()) {
          if (!row?.step_payload) continue;
          const said = strip(textFromBlob(row.step_payload));
          if (!boilerplate(said)) { preview = clip(said); break; }
        }
      }
    } catch { /* a conversation still being written */ } finally {
      try { db.close(); } catch { /* already closed */ }
    }
  }
  if (!messages) return null;
  return {
    id,
    preview: preview || '(no message)',
    cwd: hit.place.isolated ? hit.place.dir : '',
    when: hit.when.toISOString(),
    messages,
    isolated: hit.place.isolated,
  };
}

// Antigravity stores each step as protobuf. The first step is the person's
// own words, so the longest readable run in it is the preview - enough to
// recognise a conversation by, without a protobuf schema to keep in step with.
function textFromBlob(blob) {
  const text = Buffer.from(blob).toString('utf8');
  let best = '';
  for (const run of text.split(/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]+/)) {
    const s = run.replace(/^[\s\W]{0,4}/, '').trim();
    if (s.length <= best.length) continue;
    if (/^[0-9a-f-]{20,}$/i.test(s)) continue; // an id, not a sentence
    if (!/[a-z]{3}/i.test(s)) continue;
    best = s;
  }
  return best;
}

// ---------------------------------------------------------------- entry

export function listHistory(agent, roots, limit = 10) {
  const n = Math.max(1, Math.min(50, limit));
  switch (agent) {
    case 'codex': return codexHistory(roots, n);
    case 'antigravity': return agyHistory(roots, n);
    default: return claudeHistory(roots, n);
  }
}
