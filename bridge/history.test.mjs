import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { after, test } from 'node:test';
import { findHistory, listHistory } from './history.mjs';

const home = fs.mkdtempSync(path.join(os.tmpdir(), 'gateway-history-test-'));
const priorHome = process.env.HOME;
process.env.HOME = home;
after(() => {
  if (priorHome === undefined) delete process.env.HOME;
  else process.env.HOME = priorHome;
  fs.rmSync(home, { recursive: true, force: true });
});

function id(n) { return 'abcdefab-1234-5678-90ab-' + String(n).padStart(12, '0'); }

function fixture(agent, base, uuid, cwd, when = Date.now(), customName = '') {
  let file;
  if (agent === 'claude') {
    file = path.join(base, '.claude/projects/project', uuid + '.jsonl');
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, JSON.stringify({ type: 'user', cwd, message: { content: 'Work on the saved project' } }) + '\n');
  } else if (agent === 'codex') {
    file = path.join(base, '.codex/sessions/2026/01/01', customName || `rollout-2026-01-01T00-00-00-${uuid}.jsonl`);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, JSON.stringify({ type: 'session_meta', payload: { id: uuid, cwd } }) + '\n' +
      JSON.stringify({ type: 'response_item', payload: { type: 'message', role: 'user', content: [{ type: 'input_text', text: 'Continue the old project' }] } }) + '\n');
  } else {
    file = path.join(base, '.gemini/antigravity-cli/conversations', uuid + '.db');
    fs.mkdirSync(path.dirname(file), { recursive: true });
    const db = new DatabaseSync(file);
    db.exec('CREATE TABLE steps (idx INTEGER, step_payload BLOB)');
    db.prepare('INSERT INTO steps VALUES (?, ?)').run(0, Buffer.from('Work on the saved Antigravity project'));
    db.close();
  }
  fs.utimesSync(file, new Date(when), new Date(when));
  return file;
}

for (const agent of ['claude', 'codex', 'antigravity']) {
  test(`${agent}: finds an old UUID beyond recent history and preserves its folder`, () => {
    for (let n = 1; n <= 12; n++) {
      fixture(agent, home, id(n), '/saved/project-' + n, Date.now() - n * 60000);
    }
    assert.equal(listHistory(agent, [], 10).some(s => s.id === id(12)), false);
    const found = findHistory(agent, [], id(12).toUpperCase());
    assert.equal(found.length, 1);
    assert.equal(found[0].id, id(12));
    assert.equal(found[0].isolated, false);
    if (agent !== 'antigravity') assert.equal(found[0].cwd, '/saved/project-12');
    assert.deepEqual(findHistory(agent, [], id(99)), []);
  });

  test(`${agent}: searches private sandbox history in its original root`, () => {
    const root = path.join(home, 'isolated');
    const sandbox = path.join(root, agent, 'sandbox');
    fixture(agent, path.join(sandbox, '.home'), id(50), '/workspace/project');
    const found = findHistory(agent, [root], id(50));
    assert.equal(found.length, 1);
    assert.equal(found[0].isolated, true);
    assert.equal(found[0].cwd, sandbox);
    assert.deepEqual(findHistory(agent, [], id(50)), []);
  });
}

test('Codex exact lookup checks metadata in older/custom filenames', () => {
  fixture('codex', home, id(70), '/custom/filename', Date.now(), 'legacy.jsonl');
  assert.equal(findHistory('codex', [], id(70))[0].cwd, '/custom/filename');
});

test('UUID lookup rejects path input and unknown agents', () => {
  assert.throws(() => findHistory('claude', [], '../anything'), /invalid conversation UUID/);
  assert.throws(() => findHistory('other', [], id(1)), /unknown agent/);
});

test('UUID lookup never finds a different agent conversation', () => {
  fixture('claude', home, id(80), '/claude-only');
  assert.deepEqual(findHistory('codex', [], id(80)), []);
  assert.deepEqual(findHistory('antigravity', [], id(80)), []);
});
