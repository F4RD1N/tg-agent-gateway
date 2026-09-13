# Codex connection recovery update

The gateway repeated every SDK reconnect message, could report a failed turn as successful, and could print the same final error again when the CLI process exited. The worker now keeps one final outcome per turn and shows at most one reconnect notice.

`Unable to verify model access right now. Please retry.` is handled as a temporary verification failure. The gateway permits at most two additional attempts, after 2 and 5 seconds, using the same SDK Thread object, selected model, working directory, images, and cancellation signal. It retries only if no assistant/tool activity began; it does not replay commands, edits, or partial responses. The CLI may also perform its own bounded retries inside each attempt. Exhaustion yields one error and an error completion, never a successful completion.

Authentication/access denials, unsupported models, and precaution stops remain terminal. Automatic fresh-conversation recovery for precaution stops was removed. No model, authentication, access, or permission settings were changed.

A fresh bounded read-only connectivity probe with the deployed Codex SDK CLI, existing ChatGPT sign-in, and `gpt-6-astra` returned `ACCESS_OK` in 3.82 seconds with exit0. This confirms access worked at verification time; it cannot guarantee the remote service will never have another interruption.

## Verification

- `node --test bridge/codex-turn.test.mjs`: 10 regression cases passed, including duplicate errors, retry limits, thread/model/input preservation, cancellation, missing completion, denied access, and prevention of replay after work starts.
- `npm --prefix bridge run lint`: passed.
- `go test ./...`: passed; the suite now includes the Codex stream regression cases and syntax/lint checks for the new module.
- Deployed worker and helper syntax checked, hashes matched source; gateway service remained active.

## Deployment

The changed runtime files are `bridge/worker.mjs` and `bridge/codex-turn.mjs`. Copy the helper first and then atomically replace the worker. Existing worker processes retain loaded JavaScript until they end; new workers use the update immediately. Do not restart an actively running gateway or worker to load it, because that can interrupt the task. Refresh idle workers or restart the service during an idle window.

The clean source archive excludes node_modules, .git, temporary probe/deployment files, caches, credentials, and compiled output. The Linux x86_64 delivery contains the compiled gateway plus bridge sources/manifests and tools; install bridge dependencies with `npm ci --omit=dev` before using it on another host.

Official references reviewed: [Codex authentication](https://developers.openai.com/codex/auth) and [noninteractive execution](https://developers.openai.com/codex/noninteractive).

The current long-running worker refresh remains pending. Its prompt queue is private in-memory state and the running version exposes no safe idle/queue-status control. A process-tree check alone cannot prove there are no queued or just-arriving messages, so no blind timer, SIGTERM, or service restart was scheduled. The deployed update is active for new worker processes; the old loaded worker continues its authorized task until an idle refresh can be coordinated.
