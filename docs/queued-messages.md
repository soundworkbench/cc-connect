# Durable message queues

When a session is busy, queued inputs survive service restarts if `data_dir` is set. `/stop` interrupts the current turn, preserves conversation history, and continues the queue after the old agent process closes. `/new`, `/cancel`, session deletion, and other session resets discard the queue. Each discarded input receives a notice containing its first 80 Unicode characters. Longer previews end with `...`.

## Files

Each routing key uses one JSONL snapshot under `data_dir/queues/`. The filename is `<SHA256(project)>_<SHA256(interactive-session-key)>.jsonl`. The project prefix prevents projects sharing a data directory from overwriting each other's queues. Hashing the key prevents path separators from escaping the directory. Workspace routing remains part of the interactive key.

Queue files have mode `0600`. New queue directories have mode `0700`. Each line stores the input, sender, message ID, attachments, permission mode, session ID, workspace, and reply target. Files can contain private message text and attachment bytes. Platform objects, callbacks, credentials, and live processes are not serialized.

Admission replaces the snapshot with a same-directory temporary file, syncs it, renames it, and syncs the directory before acknowledging receipt. Starting a queued execution removes that input from the snapshot before sending it to the agent. Empty queues remove their snapshot. Failed writes are logged. Failed admission receives an error rather than a queue acknowledgement; failed dequeue leaves the input pending and prevents dispatch.

## Restart and shutdown

The engine reads snapshots before starting platforms. When a platform becomes ready, its `ReplyContextReconstructor` restores reply routes and the engine resumes each queue in order. A recovered backlog stays ahead of new inputs. A platform that cannot reconstruct a route leaves those inputs on disk and logs a warning. Another readiness cycle retries route reconstruction. Session IDs prevent old queues from resuming in a replacement conversation.

Malformed JSONL lines are skipped with a warning containing the file path and line number. Later valid lines still recover, including large attachment records. Other file read errors fail engine startup with context. A corrupt line cannot be recovered automatically.

SIGTERM and SIGINT already call `Engine.Stop`. Stop rejects incoming work and new scheduled work, blocks further queue consumption, and flushes the snapshots. The current turn can finish its reply within the existing shutdown drain timeout; it can be interrupted when that timeout expires. Its input is already in flight and is not replayed on restart. Queue persistence covers waiting inputs, not an exactly-once model result or platform reply. A crash between removing an input and sending it can interrupt that newly started turn without replaying it.

Inputs delivered into `ReceiveMessage` by a bridge or assistant share the same queue as platform messages. Timer and cron prompts also use this queue, preserving muted replies, their permission modes, and thread-specific reply targets. The core HTTP `/send` endpoint sends outgoing platform messages; it does not submit an agent input.

Active-turn steering remains available for agents that accept supplements. Once an input joins a durable queue, it waits for its own turn rather than being promoted to a supplement. Without `data_dir`, queues retain their in-memory storage behavior while `/stop` still preserves them.

## Verification

Run the core tests and user journeys:

```sh
go test ./core/...
go test ./core/ -run TestCUJ
go test -race ./core/ -run 'TestQueuePersistence|TestCUJ_C5|TestCUJ_B1|TestScheduledPrompt|TestEngineStop|TestSteer'
```

The regression tests cover restart order, current-input exclusion, shutdown, concurrent admission and duplicate message IDs, corrupt lines, large attachments, timer and assistant bridge inputs, write failures, `/stop` continuation, and `/new` discard previews.
