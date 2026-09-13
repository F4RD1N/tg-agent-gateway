import test from 'node:test';
import assert from 'node:assert/strict';
import { runCodexStream, isRetryableCodexError } from './codex-turn.mjs';

const access = 'Unable to verify model access right now. Please retry.';
const done = { type: 'turn.completed', usage: { input_tokens: 12, output_tokens: 3 } };
const failed = message => ({ type: 'turn.failed', error: { message } });
const error = message => ({ type: 'error', message });
const message = { type: 'item.completed', item: { type: 'agent_message', text: 'OK' } };

function setup(attempts) {
  const emitted = [], notices = [], waits = [], calls = [];
  const thread = {
    id: 'original-thread',
    model: 'gpt-6-astra',
    async runStreamed(input, { signal }) {
      calls.push({ id: this.id, model: this.model, input, signal });
      const scenario = attempts[calls.length - 1];
      assert.ok(scenario, 'unexpected extra retry');
      return { events: (async function* () {
        for (const event of scenario) {
          if (event instanceof Error) throw event;
          if (event.type === 'thread.started') thread.id = event.thread_id;
          yield event;
        }
      })() };
    },
  };
  const options = { thread, input: 'same request', signal: new AbortController().signal,
    onEvent: event => emitted.push(event), onNotice: note => notices.push(note),
    wait: async ms => { waits.push(ms); } };
  return { options, emitted, notices, waits, calls, thread };
}

test('deduplicates SDK reconnect notices and accepts only a completed turn', async () => {
  const state = setup([[error(access), error('Reconnecting...'), error(access), message, done]]);
  const result = await runCodexStream(state.options);
  assert.deepEqual(result.usage, done.usage);
  assert.equal(state.notices.length, 1);
  assert.equal(state.calls.length, 1);
  assert.deepEqual(state.emitted, [message]);
});

test('retries failed verification on the same model/thread, preserving image input and cancellation', async () => {
  const state = setup([[{ type: 'thread.started', thread_id: 'created-thread' }, failed(access), new Error('CLI exited 1')], [message, done]]);
  const input = [{ type: 'text', text: 'same request' }, { type: 'local_image', path: '/tmp/example.png' }];
  state.options.input = input;
  await runCodexStream(state.options);
  assert.equal(state.calls.length, 2);
  assert.equal(state.calls[1].id, 'created-thread');
  assert.ok(state.calls.every(call => call.model === 'gpt-6-astra' && call.input === input && call.signal === state.options.signal));
  assert.deepEqual(state.waits, [2000]);
  assert.equal(state.notices.length, 1);
});

test('caps retry attempts and reports original turn failure once instead of duplicate process exit', async () => {
  const scenario = [error(access), failed(access), new Error('Codex process exited with code 1')];
  const state = setup([scenario, scenario, scenario]);
  await assert.rejects(runCodexStream(state.options), { message: access });
  assert.equal(state.calls.length, 3);
  assert.equal(state.notices.length, 1);
  assert.deepEqual(state.waits, [2000, 5000]);
  assert.deepEqual(state.emitted, []);
});

test('turn.failed without a thrown SDK exception is a failure', async () => {
  const state = setup([[failed('Model access denied')]]);
  await assert.rejects(runCodexStream(state.options), /Model access denied/);
  assert.equal(state.calls.length, 1);
});

test('does not replay commands, file edits, or partial assistant output', async () => {
  for (const type of ['command_execution', 'file_change', 'agent_message', 'reasoning', 'mcp_tool_call']) {
    const state = setup([[{ type: 'item.started', item: { type } }, failed(access)]]);
    await assert.rejects(runCodexStream(state.options), { message: access });
    assert.equal(state.calls.length, 1, type);
  }
});

test('does not retry access denials, unsupported models, or precaution stops', async () => {
  for (const text of ['Unauthorized: connection reset', 'Forbidden: temporarily unavailable', 'Model is not supported', 'Chat stopped as a precaution. Please retry.']) {
    assert.equal(isRetryableCodexError(text), false);
    const state = setup([[failed(text)]]);
    await assert.rejects(runCodexStream(state.options), { message: text });
    assert.equal(state.calls.length, 1);
  }
});

test('empty stream never reports a successful turn', async () => {
  const state = setup([[]]);
  await assert.rejects(runCodexStream(state.options), /without a completed turn/);
});

test('recognizes a thrown pre-stream transport failure and retries once', async () => {
  const state = setup([[new Error('ECONNRESET')], [done]]);
  await runCodexStream(state.options);
  assert.equal(state.calls.length, 2);
});

test('cancellation interrupts the retry delay without starting another request', async () => {
  const state = setup([[failed(access)]]);
  const controller = new AbortController();
  state.options.signal = controller.signal;
  state.options.wait = async (_ms, signal) => {
    controller.abort();
    assert.equal(signal.aborted, true);
  };
  await assert.rejects(runCodexStream(state.options), { name: 'AbortError' });
  assert.equal(state.calls.length, 1);
});

test('an already cancelled request does not start the SDK', async () => {
  const state = setup([]);
  const controller = new AbortController();
  controller.abort();
  state.options.signal = controller.signal;
  await assert.rejects(runCodexStream(state.options), { name: 'AbortError' });
  assert.equal(state.calls.length, 0);
});
