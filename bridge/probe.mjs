// One cheap turn against each SDK, then a resumed second turn, so the
// gateway is written against behaviour that was observed rather than guessed.
import { query } from '@anthropic-ai/claude-agent-sdk';
import { Codex } from '@openai/codex-sdk';

const CWD = '/tmp';

async function probeClaude() {
  console.log('--- claude: first turn');
  let sessionId = null;
  const seen = new Set();
  const q = query({
    prompt: 'Reply with exactly the word PROBE-ONE and nothing else.',
    options: {
      cwd: CWD,
      permissionMode: 'bypassPermissions',
      allowDangerouslySkipPermissions: true,
      settingSources: [],
      includePartialMessages: true,
      env: { ...process.env, IS_SANDBOX: '1' },
    },
  });
  for await (const m of q) {
    seen.add(m.type + (m.subtype ? ':' + m.subtype : ''));
    if (m.type === 'system' && m.subtype === 'init') sessionId = m.session_id;
    if (m.type === 'assistant') {
      const text = (m.message?.content || []).filter(c => c.type === 'text').map(c => c.text).join('');
      if (text) console.log('  assistant:', JSON.stringify(text.slice(0, 120)));
    }
    if (m.type === 'stream_event') {
      const d = m.event?.delta;
      if (d?.type === 'text_delta') process.stdout.write('');
    }
    if (m.type === 'result') {
      sessionId = m.session_id || sessionId;
      console.log('  result:', m.subtype, 'cost', m.total_cost_usd, 'turns', m.num_turns);
    }
  }
  console.log('  message types:', [...seen].join(', '));
  console.log('  session:', sessionId);

  console.log('--- claude: resumed turn');
  const q2 = query({
    prompt: 'What word did you just say? Reply with only that word.',
    options: {
      cwd: CWD, resume: sessionId,
      permissionMode: 'bypassPermissions', allowDangerouslySkipPermissions: true,
      settingSources: [], env: { ...process.env, IS_SANDBOX: '1' },
    },
  });
  for await (const m of q2) {
    if (m.type === 'assistant') {
      const text = (m.message?.content || []).filter(c => c.type === 'text').map(c => c.text).join('');
      if (text) console.log('  assistant:', JSON.stringify(text.slice(0, 120)));
    }
    if (m.type === 'result') console.log('  result:', m.subtype, 'session', m.session_id);
  }
}

async function probeCodex() {
  console.log('--- codex: first turn');
  const codex = new Codex();
  const thread = codex.startThread({
    workingDirectory: CWD,
    sandboxMode: 'danger-full-access',
    approvalPolicy: 'never',
    skipGitRepoCheck: true,
  });
  const seen = new Set();
  const { events } = await thread.runStreamed('Reply with exactly the word PROBE-TWO and nothing else.');
  for await (const ev of events) {
    seen.add(ev.type + (ev.item ? ':' + ev.item.type : ''));
    if (ev.type === 'item.completed' && ev.item.type === 'agent_message') {
      console.log('  agent:', JSON.stringify(ev.item.text.slice(0, 120)));
    }
    if (ev.type === 'turn.completed') console.log('  usage:', JSON.stringify(ev.usage));
    if (ev.type === 'turn.failed') console.log('  failed:', JSON.stringify(ev.error));
    if (ev.type === 'error') console.log('  error:', ev.message);
  }
  console.log('  event types:', [...seen].join(', '));
  console.log('  thread:', thread.id);

  console.log('--- codex: resumed turn');
  const t2 = codex.resumeThread(thread.id, {
    workingDirectory: CWD, sandboxMode: 'danger-full-access', approvalPolicy: 'never', skipGitRepoCheck: true,
  });
  const r2 = await t2.runStreamed('What word did you just say? Reply with only that word.');
  for await (const ev of r2.events) {
    if (ev.type === 'item.completed' && ev.item.type === 'agent_message') {
      console.log('  agent:', JSON.stringify(ev.item.text.slice(0, 120)));
    }
  }
}

const which = process.argv[2] || 'both';
if (which === 'claude' || which === 'both') await probeClaude();
if (which === 'codex' || which === 'both') await probeCodex();
console.log('probe done');
