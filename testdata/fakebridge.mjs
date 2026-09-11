// A stand-in for the real agent bridge, used by the Go tests: same protocol,
// canned answers, no API calls.
import readline from 'node:readline';

const out = o => process.stdout.write(JSON.stringify(o) + '\n');
const running = new Map();

const rl = readline.createInterface({ input: process.stdin });
rl.on('line', line => {
  let m;
  try { m = JSON.parse(line); } catch { return; }
  switch (m.type) {
    case 'ping':
      out({ type: 'pong' });
      break;
    case 'models': {
      const agent = m.agent === 'codex' ? 'codex' : 'claude';
      const models = agent === 'codex'
        ? [
            { id: '', label: 'Default', description: '', efforts: [], default_effort: '' },
            { id: 'gpt-6-astra', label: 'GPT-6-Astra', description: 'most capable', efforts: ['low', 'medium', 'high', 'xhigh'], default_effort: 'medium' },
            { id: 'gpt-5.5', label: 'GPT-5.5', description: '', efforts: ['low', 'high'], default_effort: 'low' },
          ]
        : [
            { id: 'default', label: 'Default', description: 'opus', efforts: ['low', 'medium', 'high', 'xhigh', 'max'], default_effort: '' },
            { id: 'sonnet', label: 'Sonnet', description: '', efforts: ['low', 'high'], default_effort: '' },
            { id: 'haiku', label: 'Haiku', description: '', efforts: [], default_effort: '' },
          ];
      out({ type: 'models', sid: m.sid || '', agent, models });
      break;
    }
    case 'skills': {
      const agent = m.agent || 'claude';
      const skills = [];
      for (let i = 0; i < 14; i++) {
        skills.push({ name: `${agent}-skill-${i}`, description: 'does thing ' + i, hint: '', skill: i < 3 });
      }
      skills.push({ name: 'with-args', description: 'needs arguments', hint: '<file>', skill: false });
      out({ type: 'skills', sid: m.sid || '', agent, skills });
      break;
    }
    case 'history': {
      const agent = m.agent || 'claude';
      const sessions = [];
      for (let i = 0; i < (m.limit || 10); i++) {
        sessions.push({
          id: `${agent}-conv-${i}`,
          preview: `something ${agent} was asked to do, number ${i}`,
          cwd: m.roots && i === 1 ? `${m.roots[0]}/Claude/abc123` : '/tmp',
          when: new Date(Date.now() - i * 3600e3).toISOString(),
          messages: 4 + i,
          isolated: i === 1,
        });
      }
      out({ type: 'history', sid: m.sid || '', agent, sessions });
      break;
    }
    case 'workflows': {
      const runs = [];
      for (let i = 0; i < (m.limit || 7); i++) {
        runs.push({
          id: `wf_fake-${i}`,
          name: `rollout-${i}`,
          status: i === 0 ? 'running' : 'completed',
          when: new Date(Date.now() - i * 3600e3).toISOString(),
          live: i === 0,
          mine: true,
          summary: `what run ${i} was for`,
          error: '',
          duration_ms: 60000 + i * 1000,
          agent_count: 2 + i,
          tokens: 1000 * (i + 1),
          tool_calls: 3 + i,
          phases: ['Review', 'Verify'],
          logs: [`step one of ${i}`, `step two of ${i}`],
          agents: [
            { label: 'review:bugs', phase: 'Review', state: i === 0 ? 'running' : 'done', tool: 'Bash', note: 'go test ./...' },
            { label: 'verify:bugs', phase: 'Verify', state: 'done', tool: '', note: 'checked it' },
          ],
        });
      }
      out({ type: 'workflows', sid: m.sid || '', workflows: runs });
      break;
    }
    case 'workflow': {
      const id = m.run_id || 'wf_fake-0';
      const live = id === 'wf_fake-0' && !global.__wfFinished;
      out({ type: 'workflow', sid: m.sid || '', workflows: [{
        id, name: 'rollout-' + id.slice(-1), status: live ? 'running' : 'completed',
        when: new Date().toISOString(), live, mine: true,
        summary: 'what it was for', error: '', duration_ms: 61000,
        agent_count: 2, tokens: 4321, tool_calls: 9,
        phases: ['Review', 'Verify'], logs: ['step one', 'step two'],
        agents: [{ label: 'review:bugs', phase: 'Review', state: live ? 'running' : 'done', tool: 'Bash', note: 'go test ./...' }],
      }] });
      // The next look at this run says it finished, so a test can watch the
      // live view settle instead of redrawing for ever.
      global.__wfFinished = true;
      break;
    }
    case 'start':
    case 'clear':
    case 'stop':
      out({ type: 'ok', sid: m.sid || '' });
      break;
    case 'kill': {
      const t = running.get(m.sid);
      if (t) { clearTimeout(t); running.delete(m.sid); }
      out({ type: 'killed', sid: m.sid, message: 'killed the agent and everything it was running' });
      out({ type: 'done', sid: m.sid, session: 'sess-' + m.sid, subtype: 'killed', duration_ms: 3 });
      break;
    }
    case 'interrupt': {
      const t = running.get(m.sid);
      if (t) { clearTimeout(t); running.delete(m.sid); }
      out({ type: 'stopped', sid: m.sid, message: 'stopped' });
      out({ type: 'done', sid: m.sid, session: 'sess-' + m.sid, subtype: 'stopped', duration_ms: 5 });
      break;
    }
    case 'prompt': {
      const sid = m.sid;
      out({ type: 'started', sid, agent: m.agent || 'claude', session: 'sess-' + sid });
      if (String(m.text).includes('SLOW')) {
        // Stay running so the test can interrupt it.
        running.set(sid, setTimeout(() => {}, 60000));
        out({ type: 'delta', sid, text: 'thinking about it' });
        return;
      }
      out({ type: 'delta', sid, text: 'Hello ' });
      out({ type: 'delta', sid, text: 'world' });
      out({ type: 'tool', sid, status: 'start', name: 'Bash', detail: 'echo hi' });
      out({ type: 'tool', sid, status: 'ok', name: 'Bash', detail: 'hi' });
      // Hold briefly so the gateway flushes once while the step is showing,
      // the way a real turn does.
      running.set(sid, setTimeout(() => {
        running.delete(sid);
        out({ type: 'done', sid, session: 'sess-' + sid, cost: 0.01, duration_ms: 1234, tokens: { input: 10, output: 3 }, subtype: 'success' });
      }, 1400));
      break;
    }
  }
});
out({ type: 'hello', pid: process.pid });
