// Workflow runs, read from where Claude Code records them.
//
// A workflow is Claude orchestrating a crowd of agents over several phases.
// It writes two things: a record per run under
// <project>/<session>/workflows/wf_<id>.json, written when the run ends, and a
// folder per run under <project>/<session>/subagents/workflows/wf_<id>/ that
// fills up while it is going - one transcript per agent, appended live.
//
// So a finished run is read from its record, and a running one is read from
// the transcripts: a run folder with no record yet is a run still in progress.
// That is what makes the live view possible at all.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const TAIL = 96 * 1024; // enough of an agent's transcript to see what it is doing

function listDir(dir) {
  try { return fs.readdirSync(dir, { withFileTypes: true }); } catch { return []; }
}

function statOf(p) {
  try { return fs.statSync(p); } catch { return null; }
}

function readJSON(p) {
  try { return JSON.parse(fs.readFileSync(p, 'utf8')); } catch { return null; }
}

function clip(s, n = 120) {
  s = String(s ?? '').replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n) + '…' : s;
}

// projectSlug is how Claude names a folder's directory under ~/.claude/projects.
function projectSlug(cwd) {
  return String(cwd || '').replace(/[^A-Za-z0-9]/g, '-');
}

// tail reads the last of a file, which for a transcript is the newest part.
function tail(file, bytes = TAIL) {
  let fd;
  try {
    const st = fs.statSync(file);
    fd = fs.openSync(file, 'r');
    const size = Math.min(bytes, st.size);
    const buf = Buffer.alloc(size);
    fs.readSync(fd, buf, 0, size, st.size - size);
    return buf.toString('utf8');
  } catch {
    return '';
  } finally {
    if (fd !== undefined) try { fs.closeSync(fd); } catch { /* already closed */ }
  }
}

// ------------------------------------------------------------ finding runs

// runsUnder collects every run recorded by one session, finished or not.
function runsUnder(sessionDir, mine) {
  const out = new Map();
  const recordDir = path.join(sessionDir, 'workflows');
  for (const f of listDir(recordDir)) {
    if (!f.isFile() || !f.name.startsWith('wf_') || !f.name.endsWith('.json')) continue;
    const file = path.join(recordDir, f.name);
    const st = statOf(file);
    out.set(path.basename(f.name, '.json'), {
      id: path.basename(f.name, '.json'), record: file, dir: '', at: st ? st.mtimeMs : 0, mine,
    });
  }
  const liveDir = path.join(sessionDir, 'subagents', 'workflows');
  for (const d of listDir(liveDir)) {
    if (!d.isDirectory() || !d.name.startsWith('wf_')) continue;
    const dir = path.join(liveDir, d.name);
    const hit = out.get(d.name);
    if (hit) {
      hit.dir = dir;
      continue;
    }
    // No record yet, so this one is still going.
    const st = statOf(dir);
    out.set(d.name, { id: d.name, record: '', dir, at: st ? st.mtimeMs : 0, mine });
  }
  return [...out.values()];
}

// findRuns looks where the topic's own conversation would have put them first,
// then anywhere else in the same folder, so a run started before a resume is
// still listed.
function findRuns(cwd, ref) {
  const projects = path.join(os.homedir(), '.claude', 'projects');
  const slug = projectSlug(cwd);
  const bases = [];
  if (slug) bases.push(path.join(projects, slug));
  const out = [];
  for (const base of bases) {
    for (const s of listDir(base)) {
      if (!s.isDirectory()) continue;
      out.push(...runsUnder(path.join(base, s.name), ref ? s.name === ref : false));
    }
  }
  out.sort((a, b) => b.at - a.at);
  return out;
}

// ------------------------------------------------------------ one run

// liveAgents reads what each agent of a running workflow is doing, from the
// end of its own transcript.
function liveAgents(dir) {
  const agents = [];
  for (const f of listDir(dir)) {
    if (!f.isFile() || !f.name.endsWith('.meta.json')) continue;
    const id = f.name.replace(/^agent-/, '').replace(/\.meta\.json$/, '');
    const meta = readJSON(path.join(dir, f.name)) || {};
    const transcript = path.join(dir, `agent-${id}.jsonl`);
    const st = statOf(transcript);
    const a = {
      label: meta.description || meta.agentType || id.slice(0, 8),
      phase: meta.workflowPhase || '',
      state: 'running',
      tool: '',
      note: '',
      at: st ? st.mtimeMs : 0,
    };
    Object.assign(a, lastActivity(transcript));
    agents.push(a);
  }
  agents.sort((a, b) => b.at - a.at);
  return agents;
}

// lastActivity is the newest thing an agent said or did. An agent that has
// handed in its structured answer has finished, which is the only "done" a
// transcript shows.
function lastActivity(file) {
  const out = { tool: '', note: '' };
  let done = false;
  const text = tail(file);
  const lines = text.split('\n');
  lines.shift(); // the first line of a tail read is usually half a record
  for (const line of lines) {
    if (!line.trim()) continue;
    let rec;
    try { rec = JSON.parse(line); } catch { continue; }
    const content = rec.message?.content;
    if (!Array.isArray(content)) continue;
    for (const part of content) {
      if (part?.type === 'tool_use') {
        out.tool = part.name || '';
        if (part.name === 'StructuredOutput') done = true;
        const input = part.input || {};
        const detail = input.command || input.file_path || input.pattern || input.description || '';
        if (detail) out.note = clip(detail, 90);
        else out.note = '';
      } else if (part?.type === 'text' && part.text && part.text.trim()) {
        out.note = clip(part.text, 110);
        out.tool = '';
      }
    }
  }
  if (done) out.state = 'done';
  return out;
}

// agentsFromRecord takes the final state of each agent out of a finished run.
function agentsFromRecord(rec) {
  const out = [];
  for (const p of rec.workflowProgress || []) {
    if (p.type !== 'workflow_agent') continue;
    out.push({
      label: p.label || p.agentType || '',
      phase: p.phaseTitle || '',
      state: p.state || '',
      tool: p.lastToolName || '',
      note: clip(p.lastToolSummary || p.promptPreview || '', 110),
      at: p.lastProgressAt || p.startedAt || 0,
    });
  }
  return out;
}

// STALE is how long a run folder may sit untouched before it stops counting
// as live. A workflow that was killed outright never gets a record written,
// and without this it would look like it was still going for ever.
const STALE = 5 * 60 * 1000;

function shape(run) {
  const rec = run.record ? readJSON(run.record) : null;
  const agents = rec ? agentsFromRecord(rec) : liveAgents(run.dir);
  const touched = agents.reduce((max, a) => (a.at > max ? a.at : max), 0);
  const live = !rec && Date.now() - touched < STALE;
  const base = {
    id: run.id,
    live,
    mine: !!run.mine,
    when: new Date(run.at).toISOString(),
    name: rec?.workflowName || '',
    // No record and nothing happening for a while means it was killed before
    // it could write one: say so rather than calling it finished.
    status: rec?.status || (live ? 'running' : 'abandoned'),
    summary: rec?.summary || '',
    error: rec?.error ? clip(rec.error, 300) : '',
    duration_ms: rec?.durationMs || 0,
    agent_count: rec?.agentCount || agents.length,
    tokens: rec?.totalTokens || 0,
    tool_calls: rec?.totalToolCalls || 0,
    phases: (rec?.phases || []).map(p => p.title || '').filter(Boolean),
    logs: rec?.logs || [],
    agents,
  };
  if (!rec) {
    // A run with no record has no name of its own, so it is named by the
    // phases its agents say they are in.
    const phases = [];
    for (const a of agents) {
      if (a.phase && !phases.includes(a.phase)) phases.push(a.phase);
    }
    base.phases = phases;
    base.name = phases[0] || 'workflow';
    const started = agents.reduce((min, a) => (a.at && a.at < min ? a.at : min), touched || Date.now());
    base.duration_ms = Math.max(0, (live ? Date.now() : touched) - started);
    base.when = new Date(touched || run.at).toISOString();
  }
  return base;
}

export function listWorkflows(cwd, ref, limit = 7) {
  const n = Math.max(1, Math.min(25, limit));
  const runs = findRuns(cwd, ref);
  // A run the topic's own conversation started is the one the person asking
  // most likely means, so those come first.
  const mine = runs.filter(r => r.mine);
  const chosen = (mine.length ? mine : runs).slice(0, n);
  return chosen.map(shape);
}

export function workflowDetail(cwd, ref, runId) {
  for (const run of findRuns(cwd, ref)) {
    if (run.id === runId) return shape(run);
  }
  return null;
}
