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

## WebSocket disconnects

The September 16 report was `Falling back from WebSockets to HTTPS transport. stream disconnected before completion: websocket closed by server before response.completed`. This is a transport fallback warning, not proof the entire turn failed. Only a completed turn establishes success. The active topic worker started on September 13 at 17:44 UTC, before the recovery module was deployed at 21:47 UTC that day; its older in-memory code therefore still emitted repeated reconnect notices.

The server now selects an `openai-https` provider in the service user's Codex config. It retains OpenAI authentication, the Responses protocol, and the selected model, while setting `supports_websockets = false`. No endpoint, token, or API key is embedded. HTTPS streaming is used from the start of the next CLI invocation. This root-account config also applies to other root Codex invocations that do not explicitly select another provider. The already-running CLI keeps its current transport until it exits.

Use [examples/codex-https.toml](examples/codex-https.toml) as a merge example, preserving the existing config. With CLI 0.153.4, the built-in `openai` provider ID is reserved and cannot be overridden: use the separate ID shown in the example. The old `responses_websockets` feature flags are removed and are not a working fix. A private backup of the original server config is retained outside this source tree. To undo the transport change, remove the top-level `model_provider = "openai-https"` selection and the associated provider table, or restore that private backup while preserving any later edits.

Verification on this server:

- A bounded read-only `gpt-6-astra` HTTPS probe returned `HTTPS_OK`, emitted `turn.completed`, and exited 0 in 3.19 seconds.
- A second probe used the deployed SDK and the persistent config, resumed the first probe's thread, and checked that it retained the previous response. No production conversation was replayed.
- Twelve stream regression tests include successful HTTPS fallback and interruption after a tool starts; the latter must not replay the request.
- The installer now copies `codex-turn.mjs`, which the worker imports. Previously a clean installation could omit this helper even though the live deployment had it.

No gateway restart was performed, so queued messages and running tasks were not discarded. The old worker's notice-deduplication code still awaits a normal worker refresh; the HTTPS setting takes effect independently on its next CLI invocation. This mitigates the WebSocket failure path, not all possible network or upstream errors.

Configuration reference: [official Codex provider settings](https://learn.chatgpt.com/docs/config-file/config-reference).
