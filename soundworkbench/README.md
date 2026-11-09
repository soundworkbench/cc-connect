# soundworkbench/cc-connect: changes and maintenance

This fork carries its patches on top of the upstream cc-connect release `v1.5.0`, on the `local` branch. `main` follows upstream and has none of these changes. Executables are built only from `local`. To see every commit: `git log v1.5.0..local`.

The fork exists because several coding agents stay online in Feishu all day. That use needs a quieter progress display, messages that can join a running turn, durable queues, and scheduled turns that do not flood the chat.

## Changes

### Streaming progress card (Feishu)

Turned on with `[stream_preview] segmented = true` and `step_list = true`. The card is a Feishu CardKit streaming card (`platform/feishu/segmented_preview.go`, `core/streaming.go`).

- The card starts with one row: an animated spinner GIF and "思考中…" ("Thinking…" in English). Elements without content are left out of the card, so there are no blank lines.
- A short single-line text from the agent (up to 60 characters) is a progress sentence. It shows in the spinner row, never in the body. When the agent starts the next sentence or the step ends, the row becomes a finished step with a check icon.
- A tool call without a sentence becomes its own step, labelled with the tool's description: the Bash or Agent description, the file base name, the page host, or the search query. The current tool shows in grey under the running row.
- Finished rows and the running row share one icon slot, so all step texts line up. The card shows the last 5 finished rows and one "另有 N 步已完成" line for older ones. If the icons cannot be uploaded, the card falls back to text glyphs.
- Longer or multi-line text is the reply and goes to the card body. With `reveal_cps` set, the body is revealed at a steady rate. The event loop never waits on card API calls.
- End of turn with `end_mode = "settle"`: a turn shorter than `notify_after_ms` finishes in the card, which then holds only the reply. A longer turn collapses the card to one grey "已完成" line and sends the reply as a new message, so the phone notifies. With `end_mode = "recall"` (default) the card is recalled and the reply is sent as a new message; if the recall fails, the card becomes the reply, so the reply never shows twice.
- All cards use `width_mode = "default"` (fixed default width) instead of full width.
- `trace_file` writes per-event streaming timings as JSON lines, for tuning.

Without `step_list`, `segmented` gives a simpler streaming card: elapsed time, step count and recent steps above the typed reply, settled in place at the end.

### Other preview options

- `stream_partial_text = true` (Claude Code agent option) passes `--include-partial-messages` and streams text deltas into the preview. The deltas feed the preview only; the reply is still the final text.
- `keep_preview_on_finish = true` (Feishu option) turns the preview card into the final reply. An in-place edit does not notify, so this happens only while the card is still the newest message in the chat and the turn ran within `keep_preview_max_secs` (default 120). Otherwise the card is recalled and the reply is sent as a new message.
- `typing_marker` shows a fixed marker (for example `...`) under a running preview. The final reply carries none.

### Reactions

- The reaction on a new message (`reaction_emoji`, default `OnIt`, `"none"` turns it off) is added before the preview card is sent.
- A message injected into a running turn gets the reaction when it is accepted. When the agent has taken it in, `DONE` is added first and the old reaction is removed after, so the message is never without a reaction.
- `cc-connect send --react <emoji>` adds a reaction to the message that started the turn.

### Messages during a running turn

- Claude Code: a message sent while the agent works is written into the current turn (`agent/claudecode/steering.go`, `core/steering.go`). Images and files are saved under the work directory and the injected text carries their paths. Large images are passed by path only.
- A message that cannot be injected at once stays queued and is retried at each tool boundary, in FIFO order. An injected message is removed from the queue snapshot before it is written, so a restart never replays it.
- The first write of each turn has a deadline, `first_write_timeout` (default `60s`). On a timeout, broken pipe or short write, the process group is ended; the next message resumes the session with `--resume` and asks the user to send again. While a slow first write holds the input, an injected message is not accepted and goes to the queue instead of waiting.
- A result the CLI emits for a turn it started on its own (for example a background task notification), before it has replayed our message, does not finish our turn. Such an earlier result is dropped only when it ends silent.
- Codex: set `backend = "app_server"` in the agent options; messages join the turn through `turn/steer`.

### Queues

- Queued messages are written to `data_dir/queues/` and restored after a restart (`core/queue_store.go`). `/stop` ends only the current turn and keeps the queue; `/new` discards it and lists what was dropped. See [docs/queued-messages.md](../docs/queued-messages.md).
- Batching: consecutive queued messages of one session go to the agent as one turn, in order, each part headed with its index, source and `HH:MM` time (Asia/Shanghai). Set in the top-level `[queue_batch]` section.
- `max_turn_time` is counted again for each queued message.

### Silent replies and automated turns

- `NO_REPLY` as the whole reply sends nothing. The marker is never rendered into a card, also when it repeats (`NO_REPLYNO_REPLY`, a turn that ends twice) or when a footer or context indicator follows it.
- Automated turns (timer, cron, relay from another bot, local assistant API, heartbeat, webhook) get no placeholder and no preview card. They send the whole reply once at the end, or nothing for `NO_REPLY`. A batch that contains one human message counts as a human turn (`core/queue_batch.go`).
- Scheduled prompts carry their source, and start notices have icons (`core/scheduled_prompt.go`, `core/scheduled_start_notice.go`).
- A muted scheduled job hides only its start notice. The reply and attachments are still delivered, and the mute does not carry over to the next turn.
- `timer --at` requires a timezone offset (`+08:00`, `Z`); use `--delay` otherwise. Creation and list output also show the time in UTC+8.

### Earlier progress display (quiet mode, no step list)

- When the agent writes no text before a tool call, the card shows the tool or command in Chinese (`agent/claudecode/chinese_progress.go`, `core/tool_progress.go`, `core/shell_progress.go`).
- Progress is appended in time order and never rewritten: `progress_history = "append" | "recent"`, `tool_progress_lines` (0 turns it off), `keep_progress_card`.
- Long substantive progress text or Markdown tables are kept in the final reply, so recalling the card loses nothing: `merge_progress_into_final`, `merge_progress_min_chars`, `merge_progress_tables` (`core/merge_progress.go`).
- `final_reply_order = "recall_first"` (default) or `"send_first"`: the two orders notify differently on phones and watches.
- Codex command labels show only the program and known subcommands (`progressSubcommands` in `agent/codex/command_progress.go`); arguments and paths are never shown.

### Feishu

- `p2p_thread_isolation = true`: one session per thread in direct chats. Plain messages and quote replies stay in the existing session.
- Interactive cards and merged forwards are read as text, images and nested forwards for the agent (`platform/feishu/interactive_content.go`). Images in cards from other apps cannot be downloaded; the text says so.
- Decision cards (`core/decision.go`, opt-in with `[decision] enabled = true`): `cc-connect send --decision card.json` posts a card with one button per option. The JSON has `question`, `options` (recommended first), `note`, `no_default` and `deadline_minutes`. A tap, or the default when the deadline passes (`default_hours`, 12), is delivered back to the session as a message, and the card is rewritten so it cannot be used again.

### Usage and effort

- `/usage` for Claude Code reads the account usage API first and falls back to the terminal probe (`agent/claudecode/claude_usage_*.go`). OpenCode gets usage windows for a Grok subscription; set `usage_cmd` in the agent options (`agent/opencode/usage.go`).
- `/effort` offers the levels each model officially supports, for Claude Code and Codex (`effort_models.go`).
- Claude Code `config_dirs` (agent option): extra Claude config directories that hold transcripts, for launchers that set `CLAUDE_CONFIG_DIR` only on the child. `CLAUDE_CONFIG_DIR` from the project env or the process env is also honoured. Saved sessions are validated and listed across all of them.

### Local assistant API

The local Unix socket API (the one `cc-connect send` uses) has three more endpoints (`core/assistant_api.go`):

- `/assistant/inject`: deliver a message to an existing session, queued by source, optionally urgent.
- `/assistant/state`: read the session state.
- `/assistant/rotate`: start a new session with a handoff summary.

Requests carry a `request_id`; a repeated request returns the first result. Only existing bindings are used; no new routes are created.

### Codex background wake-up

Optional, `app_server` backend. When a Codex background sub-agent finishes, a turn is started so the parent session can handle the result. Turns and time are capped; a human `/stop` ends it. See [docs/codex-background-wakeup.md](../docs/codex-background-wakeup.md).

### Shutdown and startup

- On stop, the service waits for running turns to finish (`core/shutdown_drain.go`).
- An instance lock left by a crash can be reused (`cmd/cc-connect/instance_lock.go`).

### Code layout

- Timer, heartbeat and cron code moved from `core/engine.go` to `engine_timer.go`, `engine_heartbeat.go` and `engine_cron.go`. Behaviour is the same, but upstream changes to those parts of `engine.go` conflict on rebase.
- Live tests under `tests/release_local/` are skipped unless their environment variables are set.

None of these changes has been offered upstream. When upstream adds an equivalent feature, drop the fork's commit at the next rebase.

## Configuration reference

Keys added by the fork. Defaults in parentheses.

| Section | Key | Meaning |
|---|---|---|
| `[stream_preview]` | `segmented` (false) | Feishu CardKit streaming card |
| | `step_list` (false) | With `segmented`: step rows with spinner and check icons |
| | `end_mode` (`"recall"`) | `"settle"`: never recall; finish in the card or collapse to "已完成" |
| | `notify_after_ms` (60000) | Settle mode: turns this long or longer send the reply as a new message |
| | `reveal_cps` (0) | Reveal the reply at about this many characters per second; 0 = off |
| | `reveal_catchup_ms` (4000) | Speed the reveal up so a backlog clears within this time |
| | `drain_max_ms` (8000) | At end of turn, keep revealing the rest for at most this long |
| | `finish_catchup_ms` (3000) | Longest wait for the card to type out the reply before a recall; negative = off |
| | `dots_pause_ms` (2000) | Spinner glyph stands still while text grew within this time |
| | `print_strategy` (`"fast"`) | Client typing: `"fast"` flushes buffered text on new text, `"delay"` finishes typing it first |
| | `push_interval_ms` (200), `typing_interval_ms` (40), `typing_step` (6) | Card push cadence and client typing speed |
| | `typing_marker` (off) | Marker under a running preview |
| | `trace_file` (off) | Streaming timings as JSON lines |
| `[display]` | `progress_history`, `keep_progress_card`, `tool_progress_lines` | Quiet-mode progress (see above) |
| `[queue_batch]` | `enabled` (true), `max_messages` (20), `max_chars` (20000), `hold_seconds` (60), `immediate_sources` (`["human"]`) | Queue batching; automation waits up to `hold_seconds` for more messages |
| `[decision]` | `enabled` (false), `default_hours` (12) | Decision cards |
| Claude Code options | `stream_partial_text` (false), `first_write_timeout` (`"60s"`), `config_dirs` | See above |
| Codex options | `backend` (`"exec"`), `background_wakeup`, `background_wakeup_max_turns`, `background_wakeup_timeout_mins` | Steering and background wake-up |
| Feishu options | `keep_preview_on_finish` (false), `keep_preview_max_secs` (120), `p2p_thread_isolation` (false), `final_reply_order`, `merge_progress_into_final`, `merge_progress_min_chars`, `merge_progress_tables` | See above |

`config.example.toml` has comments for the main keys.

## Maintaining the local branch

Remotes: `upstream` is chenhg5/cc-connect, `origin` is soundworkbench/cc-connect.

```sh
git remote add upstream https://github.com/chenhg5/cc-connect.git
git fetch upstream --tags
git fetch origin
```

### Rebasing onto a new upstream release

Example: from v1.5.0 to v1.6.0.

```sh
git tag sw/v1.5.0 origin/local              # keep the old local so it can be rebuilt
git push origin sw/v1.5.0
git switch -C local origin/local
git rebase --onto v1.6.0 v1.5.0 local
```

`local` contains merge commits. `git rebase` flattens them, so conflicts that were resolved in a merge (mostly in `core/cuj_test.go` and `core/engine.go`) come back once on the commit concerned.

On a conflict:

1. Adapt the fork's commit to the new upstream code and keep its behaviour. Run the tests of the touched packages (the list in `build.sh`).
2. If `README.md` conflicts, keep the fork's version: `git checkout --theirs README.md` (during a rebase, "theirs" is the fork's commit).
3. If upstream already has the same feature, drop the commit with `git rebase --skip` and remove it from this file.
4. If a commit can neither be adapted nor dropped, stay on the current release.

With many conflicts, squash the fork first and resolve one round, at the cost of the per-commit history:

```sh
git switch -C local-squash origin/local
git reset --soft v1.5.0 && git commit -m "soundworkbench patches on v1.5.0"
git rebase --onto v1.6.0 v1.5.0 local-squash
```

Build and test with `soundworkbench/build.sh`, then push:

```sh
git push --force-with-lease origin local
```

Commit messages follow upstream: `<type>(<area>): <what the change does>`, with the problem and the approach in the body.

## Build

```sh
soundworkbench/build.sh            # build local
soundworkbench/build.sh <commit>   # build a given commit
```

The script:

1. Takes the commit's source with `git archive`, so uncommitted changes are not built.
2. Runs the tests of the touched packages in a `golang:1.25` container: `./agent/claudecode/ ./agent/codex/ ./agent/opencode/ ./core/ ./platform/feishu/ ./config/ ./cmd/cc-connect/`. On failure it stops with exit code 3.
3. Builds one executable with tags `no_web,goolm` and `CGO_ENABLED=0` into `dist/`, and prints its sha256 and version.

| Item | Format | Example |
|---|---|---|
| Version (`cc-connect --version`) | `<upstream version>-sw.<n>` | `v1.5.0-sw.73` |
| File name | `cc-connect-<version>-<os>-<arch>` | `cc-connect-v1.5.0-sw.73-linux-amd64` |

The upstream version is `git describe --tags --match 'v*'`, the nearest upstream tag below the commit, so run `git fetch upstream --tags` first. `<n>` is the number of commits from that tag to the built commit. The commit SHA is also recorded (`--version`).

Environment: `GOOS`, `GOARCH` (default linux/amd64), `CPUS` (default 4), `GO_IMAGE` (default `golang:1.25`), `OUT` (default `dist`), `SKIP_TESTS=1` (build only, for rebuilding a tested commit).

The web admin UI is not included (`no_web`). To include it, build `web/` first as in the upstream `Makefile`.

## Deploy and roll back

Deploy: put the new executable in place, keep the old one under another name, restart the service. Check:

- `cc-connect --version` shows the new version.
- Send a message to each project in Feishu: the agent replies and the progress card appears and settles or is recalled.
- `/usage` shows the usage windows.

Roll back: put the old executable back and restart. If it is gone, rebuild from the `sw/<old upstream version>` tag or the old commit (`SKIP_TESTS=1 soundworkbench/build.sh <commit>`).

Sessions, queued messages and configuration live in the data directory and are not touched by a binary swap. Keys added by a newer version are ignored by an older one.

## Known issues

- Timer output, decision deadlines and queue batch headers use UTC+8 (Asia/Shanghai). This is not configurable.
- `go build ./...` fails when `web/dist` has not been built; this is upstream behaviour. Build with `-tags no_web` or build the web UI first.
- Engine tests that run without a store path leave `core/test_ws_*.json` files behind; they are ignored by `.gitignore`.
