# soundworkbench/cc-connect：改动与维护

本分叉把补丁放在上游 cc-connect 发布版 `v1.5.0` 之上，位于 `local` 分支。`main` 跟随上游，不含这些改动。可执行文件只从 `local` 构建。查看全部提交：`git log v1.5.0..local`。

建立这个分叉，是因为多个编码 Agent 全天在飞书中在线。这种用法需要更安静的进度显示、能并入进行中回合的消息、持久的队列，以及不会刷屏的定时回合。

## 改动

### 流式进度卡（飞书）

默认已打开：只写 `[stream_preview] enabled = true`，或干脆不写这一段，飞书就用这张卡；写 `segmented = false` 或 `step_list = false` 可关。这张卡是飞书 CardKit 流式卡（`platform/feishu/segmented_preview.go`、`core/streaming.go`）。

- 卡片开头只有一行：一个转圈 GIF 动画和“思考中…”。没有内容的元素不放进卡片，所以没有空行。
- Agent 写出的简短单行文字（最多 60 个字符）先不显示。随后有工具调用，它就成为一行带勾图标的已完成步骤；文字超过一行或带换行，就是回复，进正文；回合结束时也作为回复。转圈行不会闪现这类文字，正文也不会出现进度说明。
- 没有说明的工具调用自成一步，用工具的描述做标签：Bash 或 Agent 的描述、文件基本名、网页主机名或搜索词。当前工具以灰色显示在运行行下方。
- 已完成行和运行行共用一个图标位置，所以所有步骤文字对齐。卡片显示最近 5 个已完成行，更早的合并成一行“另有 N 步已完成”。图标上传失败时，卡片改用文本符号。
- 更长或多行的文字是回复，进入卡片正文。设置 `reveal_cps` 后，正文以稳定的速度逐字显示。事件循环从不等待卡片 API 调用。
- `end_mode = "settle"` 时的回合结束：短于 `notify_after_ms` 的回合在卡片内结束，卡片只留回复。更长的回合把卡片收成一行灰色的“已完成”，并把回复作为新消息发出，让手机收到通知。`end_mode = "recall"`时，卡片被撤回，回复作为新消息发出；撤回失败时，卡片变成回复，所以回复不会出现两次。
- 所有卡片都用 `width_mode = "default"`（固定的默认宽度），不用全宽。
- `trace_file` 把每个事件的流式耗时写成 JSON 行，用于调优。

不开 `step_list` 时，`segmented` 给出更简单的流式卡：在逐字显示的回复上方显示已用时间、步骤数和最近的步骤，结束时原地定稿。

### 其他预览选项

- `stream_partial_text = true`（Claude Code Agent 选项）传入 `--include-partial-messages`，把文本增量流式送入预览。增量只用于预览；回复仍是最终文本。
- `keep_preview_on_finish = true`（飞书选项）把预览卡变成最终回复。原地编辑不会通知，所以只有在卡片仍是聊天中最新的消息、且回合在 `keep_preview_max_secs`（默认 120）内完成时才这样做。否则撤回卡片，把回复作为新消息发出。
- `typing_marker` 在运行中的预览下方显示一个固定标记（例如 `...`）。最终回复不带标记。

### 表情回应

- 新消息上的表情回应（`reaction_emoji`，默认 `OnIt`，`"none"` 表示关闭）在发出预览卡之前添加。
- 注入进行中回合的消息，在被接收时得到表情回应。Agent 读入后，先加上 `DONE`，再移除旧表情，所以消息始终有表情回应。
- `cc-connect send --react <emoji>` 给发起本回合的消息加一个表情回应。

### 回合中的消息

- Claude Code：Agent 工作时发的消息会写入当前回合（`agent/claudecode/steering.go`、`core/steering.go`）。图片和文件保存在工作目录下，注入的文本带有它们的路径。大图只传路径。
- 不能立即注入的消息留在队列中，在每个工具边界按先进先出的顺序重试。已注入的消息在写入之前就从队列快照中移除，所以重启不会重放它。
- 每个回合的第一次写入有截止时间 `first_write_timeout`（默认 `60s`）。超时、管道断开或写入不完整时，进程组被结束；下一条消息用 `--resume` 恢复会话，并请用户重发。第一次写入较慢而占住输入时，注入的消息不被接收，而是进入队列，不会等待。
- CLI 自己发起的回合（例如后台任务通知）会在它回放我们的消息之前产生结果，这个结果不会结束我们的回合。只有这样的较早结果以静默结束时才被丢弃。
- Codex：在 Agent 选项中设置 `backend = "app_server"`；消息通过 `turn/steer` 并入回合。

### 队列

- 排队消息写入 `data_dir/queues/`，重启后恢复（`core/queue_store.go`）。`/stop` 只结束当前回合并保留队列；`/new` 丢弃队列，并列出被丢弃的内容。见 [docs/queued-messages.md](../docs/queued-messages.md)。
- 合并：同一会话中连续排队的消息按顺序作为一个回合发给 Agent，每一段的标题带序号、来源和 `HH:MM` 时间（Asia/Shanghai）。在顶层的 `[queue_batch]` 段设置。
- `max_turn_time` 对每条排队消息重新计时。

### 静默回复和自动回合

- 整个回复只有 `NO_REPLY` 时，不发任何内容。这个标记从不渲染进卡片；重复出现（`NO_REPLYNO_REPLY`，即一个回合结束两次）或后面跟着页脚或上下文指示时也一样。
- 自动回合（定时器、cron、其他机器人转来的消息、本地助手 API、心跳、webhook）没有占位消息，也没有预览卡。它们在结束时一次发出整个回复；回复为 `NO_REPLY` 则不发。含一条人工消息的批次算作人工回合（`core/queue_batch.go`）。
- 定时提示带有来源，开始通知带有图标（`core/scheduled_prompt.go`、`core/scheduled_start_notice.go`）。
- 被静音的定时任务只隐藏开始通知。回复和附件照常送达，静音不会延续到下一个回合。
- `timer --at` 必须带时区偏移（`+08:00`、`Z`）；否则用 `--delay`。创建和列表的输出也显示 UTC+8 时间。

### 早期的进度显示（quiet 模式，无步骤列表）

- Agent 在工具调用前没有写文字时，卡片用中文显示工具或命令（`agent/claudecode/chinese_progress.go`、`core/tool_progress.go`、`core/shell_progress.go`）。
- 进度按时间顺序追加，从不改写：`progress_history = "append" | "recent"`、`tool_progress_lines`（0 表示关闭）、`keep_progress_card`。
- 较长的实质性进度文字或 Markdown 表格保留在最终回复中，所以撤回卡片不会丢失内容：`merge_progress_into_final`、`merge_progress_min_chars`、`merge_progress_tables`（`core/merge_progress.go`）。
- `final_reply_order = "recall_first"`（默认）或 `"send_first"`：两种顺序在手机和手表上的通知方式不同。
- Codex 命令标签只显示程序名和已知子命令（`agent/codex/command_progress.go` 中的 `progressSubcommands`）；参数和路径从不显示。

### 飞书

- `p2p_thread_isolation = true`：私聊中每个话题一个会话。普通消息和引用回复留在已有会话中。
- 交互卡片和合并转发被读成文本、图片和嵌套转发，供 Agent 使用（`platform/feishu/interactive_content.go`）。来自其他应用的卡片中的图片无法下载；文本中会说明这一点。
- 决策卡（`core/decision.go`，用 `[decision] enabled = true` 开启）：`cc-connect send --decision card.json` 发出一张卡片，每个选项一个按钮。JSON 含 `question`、`options`（推荐项在前）、`note`、`no_default` 和 `deadline_minutes`。一次点选，或截止时间到后的默认选项（`default_hours`，12），会作为消息回到会话，卡片被改写，不能再次使用。

### 用量和推理强度

- Claude Code 的 `/usage` 先读账号用量 API，失败时回退到终端探测（`agent/claudecode/claude_usage_*.go`）。OpenCode 为 Grok 订阅提供用量窗口；在 Agent 选项中设置 `usage_cmd`（`agent/opencode/usage.go`）。
- `/effort` 为 Claude Code 和 Codex 列出每个模型官方支持的强度级别（`effort_models.go`）。
- Claude Code 的 `config_dirs`（Agent 选项）：存放对话记录的额外 Claude 配置目录，用于只在子进程上设置 `CLAUDE_CONFIG_DIR` 的启动器。项目环境或进程环境中的 `CLAUDE_CONFIG_DIR` 也会被采用。已保存的会话会在所有这些目录中校验并列出。

### 本地助手 API

本地 Unix socket API（`cc-connect send` 使用的那个）多了三个端点（`core/assistant_api.go`）：

- `/assistant/inject`：向已有会话投递一条消息，按来源排队，可设为紧急。
- `/assistant/state`：读取会话状态。
- `/assistant/rotate`：用交接摘要开始一个新会话。

请求带 `request_id`；重复的请求返回第一次的结果。只使用已有的绑定，不创建新路由。

### Codex 后台唤醒

可选，需要 `app_server` 后端。Codex 的后台子 Agent 结束时，会启动一个回合，让父会话处理结果。回合数和时间有上限；人工 `/stop` 会结束它。见 [docs/codex-background-wakeup.md](../docs/codex-background-wakeup.md)。

### 关闭和启动

- 停止时，服务等待运行中的回合结束（`core/shutdown_drain.go`）。
- 崩溃后遗留的实例锁可以复用（`cmd/cc-connect/instance_lock.go`）。

### 代码布局

- 定时器、心跳和 cron 的代码从 `core/engine.go` 移到 `engine_timer.go`、`engine_heartbeat.go` 和 `engine_cron.go`。行为不变，但上游对 `engine.go` 这些部分的改动在变基时会冲突。
- `tests/release_local/` 下的实机测试，在未设置其环境变量时会跳过。

这些改动都没有提交给上游。上游加入等效功能后，在下次变基时丢弃本分叉的对应提交。

## 配置项参考

本分叉新增的配置项。括号内是默认值。

| 段 | 键 | 含义 |
|---|---|---|
| `[stream_preview]` | `segmented` (true) | 飞书 CardKit 流式卡 |
| | `step_list` (true) | 与 `segmented` 一起用：带转圈和勾图标的步骤行 |
| | `end_mode` (`"settle"`) | `"settle"`：从不撤回；在卡片内结束，或收成“已完成” |
| | `notify_after_ms` (60000) | settle 模式：达到或超过这个时长的回合，把回复作为新消息发出 |
| | `reveal_cps` (90) | 回复逐字显示的速度，约每秒这么多个字符；写 0 关闭 |
| | `reveal_catchup_ms` (4000) | 加快显示，使积压在这个时间内清完 |
| | `reveal_lead_ms` (250) | 让客户端的打字缓冲保持这么多毫秒的打字量，卡片调用偶尔变慢时不会断流；写 0 关闭 |
| | `body_inflight` (6) | 正文更新同时在途的最大个数，每 `push_interval_ms` 发一次、不等上一次返回；0 或 1 表示一次一个。单张卡片每秒操作上限 10 次，`push_interval_ms` 不要低于 100 |
| | `drain_max_ms` (8000) | 回合结束时，继续显示剩余内容的最长时间 |
| | `finish_catchup_ms` (3000) | 撤回前等待卡片打完回复的最长时间；负数表示关闭 |
| | `dots_pause_ms` (2000) | 文本在这个时间内增长过，转圈符号就停住不动 |
| | `print_strategy` (`"delay"`) | 客户端打字方式：`"fast"` 在有新文本时立即输出缓冲文本，`"delay"` 先打完缓冲文本 |
| | `push_interval_ms` (150)、`typing_interval_ms` (40)、`typing_step` (4) | 卡片推送节奏和客户端打字速度 |
| | `typing_marker` (飞书 `"..."`，其他平台关) | 运行中的预览下方的标记；写了就对所有平台生效 |
| | `interval_ms` (飞书 700，其他平台 1500)、`min_delta_chars` (飞书 12，其他 30)、`max_chars` (飞书 4000，其他 2000) | 预览更新节奏和长度；写了就对所有平台生效 |
| | `trace_file` (off) | 流式耗时，写成 JSON 行 |
| agent 选项 | `stream_partial_text` (含飞书平台的 claudecode 项目为 true) | 卡片显示正在写的文本块；写 `false` 关闭 |
| `[display]` | `progress_history`、`keep_progress_card`、`tool_progress_lines` | quiet 模式的进度（见上文） |
| `[queue_batch]` | `enabled` (true)、`max_messages` (20)、`max_chars` (20000)、`hold_seconds` (60)、`immediate_sources` (`["human"]`) | 队列合并；自动消息最多等待 `hold_seconds` 秒，看是否有更多消息 |
| `[decision]` | `enabled` (false)、`default_hours` (12) | 决策卡 |
| Claude Code 选项 | `stream_partial_text` (false)、`first_write_timeout` (`"60s"`)、`config_dirs` | 见上文 |
| Codex 选项 | `backend` (`"exec"`)、`background_wakeup`、`background_wakeup_max_turns`、`background_wakeup_timeout_mins` | 回合并入和后台唤醒 |
| 飞书选项 | `keep_preview_on_finish` (false)、`keep_preview_max_secs` (120)、`p2p_thread_isolation` (false)、`final_reply_order`、`merge_progress_into_final`、`merge_progress_min_chars`、`merge_progress_tables` | 见上文 |

`config.example.toml` 为主要配置项提供了注释。

## 维护 local 分支

远程仓库：`upstream` 是 chenhg5/cc-connect，`origin` 是 soundworkbench/cc-connect。

```sh
git remote add upstream https://github.com/chenhg5/cc-connect.git
git fetch upstream --tags
git fetch origin
```

### 变基到新的上游版本

示例：从 v1.5.0 到 v1.6.0。

```sh
git tag sw/v1.5.0 origin/local              # keep the old local so it can be rebuilt
git push origin sw/v1.5.0
git switch -C local origin/local
git rebase --onto v1.6.0 v1.5.0 local
```

`local` 含有合并提交。`git rebase` 会把它们展平，所以原先在合并中解决过的冲突（主要在 `core/cuj_test.go` 和 `core/engine.go`）会在相关的提交上再出现一次。

遇到冲突时：

1. 让本分叉的提交适配新的上游代码，并保持它的行为。运行涉及的包的测试（列表见 `build.sh`）。
2. 如果 `README.md` 冲突，保留本分叉的版本：`git checkout --theirs README.md`（在变基中，“theirs”是本分叉的提交）。
3. 如果上游已有同样的功能，用 `git rebase --skip` 丢弃该提交，并从本文件中删除它。
4. 如果一个提交既不能适配也不能丢弃，就停留在当前版本。

冲突很多时，先把本分叉压成一个提交，一次解决，代价是丢失逐个提交的历史：

```sh
git switch -C local-squash origin/local
git reset --soft v1.5.0 && git commit -m "soundworkbench patches on v1.5.0"
git rebase --onto v1.6.0 v1.5.0 local-squash
```

用 `soundworkbench/build.sh` 构建并测试，然后推送：

```sh
git push --force-with-lease origin local
```

提交信息沿用上游格式：`<type>(<area>): <改动做了什么>`，正文写问题和方法。

## 构建

```sh
soundworkbench/build.sh            # build local
soundworkbench/build.sh <commit>   # build a given commit
```

脚本的步骤：

1. 用 `git archive` 取出该提交的源码，所以未提交的改动不会被构建。
2. 在 `golang:1.25` 容器中运行涉及的包的测试：`./agent/claudecode/ ./agent/codex/ ./agent/opencode/ ./core/ ./platform/feishu/ ./config/ ./cmd/cc-connect/`。失败时以退出码 3 停止。
3. 用标签 `no_web,goolm` 和 `CGO_ENABLED=0` 在 `dist/` 下构建一个可执行文件，并打印它的 sha256 和版本。

| 项目 | 格式 | 示例 |
|---|---|---|
| 版本（`cc-connect --version`） | `<上游版本>-sw.<n>` | `v1.5.0-sw.73` |
| 文件名 | `cc-connect-<版本>-<os>-<arch>` | `cc-connect-v1.5.0-sw.73-linux-amd64` |

上游版本取自 `git describe --tags --match 'v*'`，即该提交之下最近的上游标签，所以请先运行 `git fetch upstream --tags`。`<n>` 是从该标签到被构建提交的提交数。提交 SHA 也会被记录（`--version`）。

环境变量：`GOOS`、`GOARCH`（默认 linux/amd64）、`CPUS`（默认 4）、`GO_IMAGE`（默认 `golang:1.25`）、`OUT`（默认 `dist`）、`SKIP_TESTS=1`（只构建，用于重建已测试过的提交）。

不含 Web 管理界面（`no_web`）。要包含它，先按上游 `Makefile` 构建 `web/`。

## 部署与回滚

部署：放入新的可执行文件，把旧的另存为别的名字，重启服务。然后检查：

- `cc-connect --version` 显示新版本。
- 在飞书中向每个项目发一条消息：Agent 有回复，进度卡出现，并定稿或被撤回。
- `/usage` 显示用量窗口。

回滚：放回旧的可执行文件并重启。如果旧文件已经没有，就从 `sw/<旧的上游版本>` 标签或旧提交重新构建（`SKIP_TESTS=1 soundworkbench/build.sh <commit>`）。

会话、排队消息和配置都在数据目录中，替换可执行文件不会动它们。新版本新增的配置键，旧版本会忽略。

## 已知问题

- 定时器输出、决策截止时间和队列合并的标题使用 UTC+8（Asia/Shanghai）。这一点不可配置。
- `web/dist` 未构建时，`go build ./...` 会失败；这是上游的行为。用 `-tags no_web` 构建，或先构建 Web 界面。
- 不带存储路径运行的引擎测试会遗留 `core/test_ws_*.json` 文件；`.gitignore` 已忽略它们。
