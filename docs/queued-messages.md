# Durable message queues

When a session is busy, queued inputs survive service restarts if `data_dir` is set. `/stop` interrupts the current turn, preserves conversation history, and continues the queue after the old agent process closes. `/new`, `/cancel`, session deletion, and other session resets discard the queue. Each discarded input receives a notice containing its first 80 Unicode characters. Longer previews end with `...`.

## Batching and the idle window

The top-level `[queue_batch]` section applies to every project. Changes take effect after restart.

```toml
[queue_batch]
enabled = true
max_messages = 20
max_chars = 20000
hold_seconds = 3
immediate_sources = ["human"]
```

`enabled = false` preserves individual queued sends and disables the idle window. `hold_seconds = 0` disables the idle window while retaining queue batching. Both limits must be positive. `hold_seconds` must fit a non-negative Go duration.

At dequeue, consecutive compatible messages share one `AgentSession.Send`. They keep FIFO order, all images, and all files. Each part starts with a localized message index, source label, and `HH:MM` time. Source labels include the scheduler occurrence ID or platform and sender when available. Time is shown in Beijing time (Asia/Shanghai; local time if the zone database is unavailable) and uses the platform creation timestamp when set, otherwise the admission time. Old snapshots without either timestamp use `?`.

`max_chars` counts Unicode characters in the combined content, including markers and separators. A batch stops before the next part would exceed either limit. Remaining parts stay in the durable FIFO. A single oversized input is sent unchanged to avoid truncation or starvation. Slash commands, shell commands, permission callbacks, preflight inputs, and messages with `ModeOverride` form individual boundaries. They are never moved past another queued input. Recognized commands still use the existing command path, including `/stop` interruption.

The final message supplies the reply platform and reply context. Earlier parts with a different context receive the platform's existing received/done reaction or done acknowledgement when supported. This acknowledges that their answer is included in the final message's reply. Platforms without this capability skip the acknowledgement. The batch is muted only when every part is muted, so a human message releases waiting muted automation into a visible reply. History stores the combined user content once. Existing session-save points are unchanged.

When a session is idle, automation enters the durable FIFO before waiting for `hold_seconds`. The first arrival sets the deadline; later arrivals do not extend it. A human or urgent message wakes that wait and joins the same FIFO. Decision-card answers and restart resumes are urgent, so they start as soon as the session is idle. Normal batch limits and individual-message boundaries still apply.

Source classification uses these fields in order:

- An explicit `Message.Source`, normalized to lowercase, identifies relay or other automation.
- Internal `scheduledPrompt` inputs use the prefix of `scheduledInputID`: `timer`, `cron`, `assistant`, or `rotation`. Other scheduled inputs use `scheduled`.
- A user ID of `relay` or starting with `relay:` identifies relay input.
- Other platform input is `human`, which always bypasses the wait. Unknown platform senders therefore receive immediate service.

Feishu adapters classify SDK sender type `app` as `relay`; real user events remain `human`. Core does not inspect platform names. `immediate_sources` adds automation sources that bypass waiting, compared without case sensitivity. `human`, rotation/preflight inputs, commands, and permission-mode overrides always bypass it.

The local `/assistant/inject` API and bridge message JSON accept optional `source` and `urgent` fields. For example, `"source": "relay", "urgent": true` submits an immediate role message. `urgent` is an explicit boolean, not inferred from free text. An urgent arrival wakes an existing idle wait. Synchronous `HandleRelay` uses its dedicated request/result session and retains separate replies; it does not enter this FIFO. Role messages delivered through the bridge, platform app events, or assistant injection use the window.

Shutdown wakes idle waiters without dequeuing their inputs. `/stop` continues the preserved queue immediately after closing the old process. `/new` and `/cancel` discard it through the existing reset path. Restored durable backlogs drain immediately after platform readiness. Source, admission time, urgency, and separate-reply boundaries survive snapshot recovery.

Successful multi-message dequeues emit `processing queued messages batched` with `session`, `count`, and `chars`. This log measures combined content size, not token savings.

## Files

Each routing key uses one JSONL snapshot under `data_dir/queues/`. The filename is `<SHA256(project)>_<SHA256(interactive-session-key)>.jsonl`. The project prefix prevents projects sharing a data directory from overwriting each other's queues. Hashing the key prevents path separators from escaping the directory. Workspace routing remains part of the interactive key.

Queue files have mode `0600`. New queue directories have mode `0700`. Each line stores the input, sender, message ID, attachments, permission mode, session ID, workspace, and reply target. Files can contain private message text and attachment bytes. Platform objects, callbacks, credentials, and live processes are not serialized.

Admission replaces the snapshot with a same-directory temporary file, syncs it, renames it, and syncs the directory before acknowledging receipt. Starting a queued execution removes its consecutive batch from the snapshot in one write before sending it to the agent. Empty queues remove their snapshot. Failed writes are logged. Failed admission receives an error rather than a queue acknowledgement; failed dequeue leaves the input pending and prevents dispatch.

## Restart and shutdown

The engine reads snapshots before starting platforms. When a platform becomes ready, its `ReplyContextReconstructor` restores reply routes and the engine resumes each queue in order. A recovered backlog stays ahead of new inputs. A platform that cannot reconstruct a route leaves those inputs on disk and logs a warning. Another readiness cycle retries route reconstruction. Session IDs prevent old queues from resuming in a replacement conversation.

Malformed JSONL lines are skipped with a warning containing the file path and line number. Later valid lines still recover, including large attachment records. Other file read errors fail engine startup with context. A corrupt line cannot be recovered automatically.

SIGTERM and SIGINT already call `Engine.Stop`. Stop rejects incoming work and new scheduled work, blocks further queue consumption, and flushes the snapshots. The current turn can finish its reply within the existing shutdown drain timeout; it can be interrupted when that timeout expires. Its input is already in flight and is not replayed on restart. Queue persistence covers waiting inputs, not an exactly-once model result or platform reply. A crash between removing an input and sending it can interrupt that newly started turn without replaying it.

Inputs delivered into `ReceiveMessage` by a bridge or assistant share the same queue as platform messages. Timer and cron prompts also use this queue, preserving muted replies, their permission modes, and thread-specific reply targets. The core HTTP `/send` endpoint sends outgoing platform messages; it does not submit an agent input.

The local `/assistant/inject` API uses the same durable FIFO. Waiting assistant inputs recover after restart and continue after `/stop`. Daily `/assistant/rotate` starts a candidate and sends its handoff summary before activating the new session. Once activated, the candidate uses the new session ID and normal routing key for queue snapshots. The summary is already in flight and is not replayed on recovery. If `/stop` replaces the state before the preflight event reader starts, that reader exits without consuming the replacement process's events.

Acceptance callbacks run after admission and queue locks are released. A callback can synchronously submit another message or `/stop`, including before the first agent process starts.

Active-turn steering remains available for agents that accept supplements. Human inputs that cannot be injected at admission stay queued and are retried in FIFO order at each tool boundary, with or without `data_dir`; an injected input is removed from the snapshot before it is written to the agent, so a restart never replays it. Images and files are saved under the work directory and the injected text carries their paths. Consecutive compatible inputs that stay queued can share one turn. Without `data_dir`, queues retain their in-memory storage behavior while `/stop` still preserves them.

## Verification

Run the core tests and user journeys:

```sh
go test ./core/... -count=1 -timeout 10m
go test ./core/ -run '^TestQueuePersistence_' -count=5 -timeout 10m
go test -race ./core/... -run '(?i)(queue|stop|schedul|timer|cron|cuj|assistant|preflight|steer)' -count=1 -timeout 10m
```

The regression tests cover restart order, current-input exclusion, shutdown, concurrent admission and duplicate message IDs, corrupt lines, large attachments, timer and assistant bridge inputs, write failures, `/stop` continuation, and `/new` discard previews. They also cover synchronous acceptance callbacks, real Unix API injection and rotation recovery, and stopped preflight readers.
