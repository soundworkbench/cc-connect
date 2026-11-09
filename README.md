# soundworkbench/cc-connect

A fork of [chenhg5/cc-connect](https://github.com/chenhg5/cc-connect). cc-connect connects coding agents such as Claude Code and Codex to chat apps. This fork runs the upstream release v1.5.0 plus a set of patches, mostly for Feishu/Lark: a streaming progress card, messages that join a running turn, message queues, and scheduled or automated turns. It is built as a single executable and has been in daily use with several long-running agents.

For installation and the features that come from upstream, read the [upstream README](https://github.com/chenhg5/cc-connect#readme).

## What you see in Feishu/Lark

With the step-list card turned on (see [Configuration](#configuration)):

- **Reaction first.** Your message gets an emoji reaction as soon as the bot picks it up, before any card appears.
- **One progress card per turn.** The card opens with a single row: an animated spinner and "思考中…" ("Thinking…" in English). There are no empty lines above or below it.
- **Steps.** When the agent writes a short progress sentence ("Reading the config"), it shows in the spinner row. When the agent moves on, the row becomes a finished step with a check icon. Tool calls without a sentence are labelled with what they do (the Bash description, a file name, a page host, a search query). Spinner and check icons share one slot, so all step texts start at the same left edge. The card shows the last five finished steps and one "另有 N 步已完成" line for the rest.
- **Reply in the body.** Only the reply goes into the card body, below the steps. Progress sentences never appear in the body. The reply is typed out at a steady pace while the agent writes it.
- **End of turn.** For a short turn, the card settles to the reply only. For a long turn, the card shrinks to one grey "已完成" line and the reply is sent as a new message, so your phone notifies you. Nothing is recalled.
- **Card width.** All cards use the default card width, not full width, including the settled "已完成" card.
- **Messages during a turn.** A message you send while the agent is working goes into the running turn instead of waiting for the turn to end. Images and files are included: they are saved in the work directory and the agent gets their paths. The message gets a reaction when it is accepted, and the reaction changes to DONE when the agent has taken it in. A message that cannot be injected at once is queued and retried at the next tool boundary.
- **Queued messages.** Messages that wait behind a turn are sent to the agent as one turn, in order, each marked with its source and time.
- **Silent replies.** An agent can answer `NO_REPLY` to say nothing. The marker is never shown, not in the card and not as a message, also when it is repeated (`NO_REPLYNO_REPLY`) or followed by a footer.
- **Automated turns.** Scheduled prompts, relays from other bots and messages from the local assistant API do not open a progress card. They send one message at the end, or nothing for `NO_REPLY`. A muted scheduled job hides only its start notice; the reply and attachments still arrive.
- **Decision cards** (opt-in, `[decision] enabled = true`). An agent can send a card with buttons (`cc-connect send --decision card.json`). Your tap, or the default when the deadline passes, comes back to the session as a message.

Other changes: one session per thread in direct chats (optional), interactive cards and merged forwards are read as text, queued messages survive a restart, `/stop` ends only the current turn, usage windows in `/usage`, per-model effort levels, a local assistant API, and Codex background-task wake-up. The full list with config keys and source files is in [soundworkbench/README.md](soundworkbench/README.md).

## Configuration

The step-list card is off by default. The settings below give the behaviour described above.

```toml
[display]
mode = "quiet"
thinking_messages = true
tool_messages = false

[stream_preview]
enabled = true
segmented = true          # streaming card (Feishu CardKit)
step_list = true          # step rows with spinner and check icons
end_mode = "settle"       # never recall the card; "recall" is the default
notify_after_ms = 60000   # turns at least this long send the reply as a new message
reveal_cps = 90           # type the reply at about this many characters per second; 0 = off
print_strategy = "delay"  # "fast" (default) or "delay"

[[projects]]
name = "my-project"

[projects.agent]
type = "claudecode"

[projects.agent.options]
stream_partial_text = true   # stream Claude text deltas into the card

[[projects.platforms]]
type = "feishu"

[projects.platforms.options]
# app_id / app_secret as in upstream
reaction_emoji = "OnIt"         # reaction on new messages; "none" turns it off
keep_preview_on_finish = false
p2p_thread_isolation = true     # optional: one session per thread in direct chats
```

Queue batching is on by default and is set in a top-level `[queue_batch]` section. See [soundworkbench/README.md](soundworkbench/README.md#configuration-reference) for all keys and defaults, and `config.example.toml` for comments.

## Branches

- `local`: the upstream release tag `v1.5.0` plus the fork's commits. Build from this branch.
- `main`: follows upstream, without the fork's changes.

## Build

```sh
soundworkbench/build.sh            # build local
soundworkbench/build.sh <commit>   # build a given commit
```

The script runs the tests of the changed packages in a `golang:1.25` Docker container and stops if they fail. Then it builds one Linux executable into `dist/`, versioned `<upstream version>-sw.<n>`, for example `v1.5.0-sw.73`. Details are in [soundworkbench/README.md](soundworkbench/README.md#build).

## Updating to a new upstream release

Rebase `local` onto the new upstream tag and tag the old `local` first, so you can rebuild it. The steps and the usual conflicts are in [soundworkbench/README.md](soundworkbench/README.md#maintaining-the-local-branch).

## 中文简介

这是 cc-connect 的分叉，在上游 v1.5.0 上加了一组补丁，主要改进飞书：流式进度卡（一行转圈的思考行、打勾的步骤行、正文只放回复、结束时收成回复或一行灰色“已完成”）、回合中发的消息（含图片和文件）直接并入当前回合、排队消息合并成一个回合、`NO_REPLY` 不发消息、定时和自动消息不开进度卡。配置见上面的 Configuration，改动清单和维护步骤见 [soundworkbench/README.md](soundworkbench/README.md)。

## License

MIT, the same as upstream. See [LICENSE](LICENSE). cc-connect is written by its upstream authors at [chenhg5/cc-connect](https://github.com/chenhg5/cc-connect).
