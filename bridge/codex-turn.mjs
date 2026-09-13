// A streamed SDK call can report a failed turn and then throw again when its
// child process exits. Keep those as one failure and only retry a request that
// has not begun producing agent work, so commands and edits are never replayed.
import { setTimeout as delay } from 'node:timers/promises';

const TRANSIENT = /unable to verify model access right now|reconnect(?:ing)?|stream disconnected|websocket closed|falling back from websockets|connection (?:reset|closed|timed out)|\bECONNRESET\b|\bETIMEDOUT\b|\bEAI_AGAIN\b|temporarily unavailable|unexpected status (?:code:?\s*)?(?:502|503|504)\b/i;
const TERMINAL = /stopped as a precaution|acting safely|resume another chat|unauthori[sz]ed|forbidden|access denied|not (?:allowed|authorized)|insufficient quota|model[^\n]*(?:not found|does not exist|not supported)/i;

export function isRetryableCodexError(message) {
  const text = String(message || '');
  return !TERMINAL.test(text) && TRANSIENT.test(text);
}

function checkAbort(signal) {
  if (signal?.aborted) {
    const error = new Error('Codex request aborted');
    error.name = 'AbortError';
    throw error;
  }
}

export async function runCodexStream({ thread, input, signal, onEvent, onNotice,
  retryDelays = [2000, 5000], wait = (ms, abortSignal) => delay(ms, undefined, { signal: abortSignal }) }) {
  let notified = false;
  const noticeOnce = () => {
    if (!notified) {
      notified = true;
      onNotice('Codex has a temporary connection or model-verification problem; retrying this conversation.');
    }
  };

  for (let attempt = 0; ; attempt++) {
    checkAbort(signal);
    let lastError = null;
    let turnFailure = null;
    let completed = false;
    let usage = null;
    let sawWork = false;
    let processFailure = null;
    try {
      // Reuse this exact Thread object: the SDK updates its id from
      // thread.started, including when the first attempt creates the thread.
      const { events } = await thread.runStreamed(input, { signal });
      for await (const event of events) {
        checkAbort(signal);
        if (event.type === 'error' || (event.type.startsWith('item.') && event.item?.type === 'error')) {
          lastError = event.message || event.item?.message || 'Codex reported an error';
          // These events often describe retries already in progress inside
          // the CLI. Do not turn each one into a Telegram error message.
          if (isRetryableCodexError(lastError)) noticeOnce();
          continue;
        }
        if (event.type === 'turn.failed') {
          turnFailure = event.error?.message || 'Codex turn failed';
          continue; // Drain the child process and absorb its duplicate exit error.
        }
        if (event.type === 'turn.completed') {
          completed = true;
          usage = event.usage || null;
          continue;
        }
        if (event.type.startsWith('item.')) sawWork = true;
        onEvent(event);
      }
    } catch (error) {
      checkAbort(signal);
      processFailure = error;
    }
    if (completed && !turnFailure && !processFailure) return { usage };
    const message = turnFailure || lastError || processFailure?.message || 'Codex stream ended without a completed turn';
    if (sawWork || attempt >= retryDelays.length || !isRetryableCodexError(message)) {
      throw new Error(message);
    }
    noticeOnce();
    await wait(retryDelays[attempt], signal);
  }
}
