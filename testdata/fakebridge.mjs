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
            { id: 'default', label: 'Default (recommended)', description: 'opus', efforts: ['low', 'medium', 'high', 'xhigh', 'max'], default_effort: '' },
            { id: 'sonnet', label: 'Sonnet', description: '', efforts: ['low', 'high'], default_effort: '' },
            { id: 'haiku', label: 'Haiku', description: '', efforts: [], default_effort: '' },
          ];
      out({ type: 'models', sid: m.sid || '', agent, models });
      break;
    }
    case 'start':
    case 'clear':
    case 'stop':
      out({ type: 'ok', sid: m.sid || '' });
      break;
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
      out({ type: 'done', sid, session: 'sess-' + sid, cost: 0.01, duration_ms: 1234, tokens: { input: 10, output: 3 }, subtype: 'success' });
      break;
    }
  }
});
out({ type: 'hello', pid: process.pid });
