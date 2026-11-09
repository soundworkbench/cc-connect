# soundworkbench/cc-connect

这是 [chenhg5/cc-connect](https://github.com/chenhg5/cc-connect) 的分叉。cc-connect 把 Claude Code、Codex 等编码 Agent 接到聊天应用。本分叉基于上游 v1.5.0 版本，加了一组补丁，主要改进飞书（Feishu/Lark）：流式进度卡、并入进行中回合的消息、消息队列，以及定时和自动触发的回合。它编译为单个可执行文件，已在多个长期运行的 Agent 上日常使用。

安装方法和上游自带的功能，见[上游 README](https://github.com/chenhg5/cc-connect#readme)。

## 飞书中的表现

开启步骤列表卡后（见[配置](#配置)）：

- **先给表情。** 机器人收到消息后，立即给你的消息加一个表情回应，然后才出现卡片。
- **每个回合一张进度卡。** 卡片开头只有一行：一个转圈动画和“思考中…”。上下没有空行。
- **步骤。** Agent 在调用工具前写的一句简短进度说明（如“正在读取配置”），在工具开始时作为一行带勾图标的已完成步骤出现；转圈行一直显示“思考中…”或当前工具的标签，不会闪现还没确定用途的文字。没有说明的工具调用，用它的作用做标签（Bash 的描述、文件名、网页主机名或搜索词）。转圈图标和勾图标共用一个位置，所以所有步骤文字左边对齐。卡片显示最近 5 个已完成步骤，更早的合并成一行“另有 N 步已完成”。
- **回复放在正文。** 只有回复进入卡片正文，位于步骤下方。进度说明不会出现在正文。Agent 写回复时，正文以稳定的速度逐字显示。
- **回合结束。** 短回合结束后，卡片只留回复。长回合结束后，卡片收成一行灰色的“已完成”，回复作为新消息发出，让手机收到通知。不撤回任何消息。
- **卡片宽度。** 所有卡片都用默认宽度，不用全宽，包括收起后的“已完成”卡。
- **回合中发消息。** Agent 工作时你发的消息会并入当前回合，不必等回合结束。图片和文件也一并处理：它们保存在工作目录，Agent 收到它们的路径。消息被接收时加一个表情回应；Agent 读入后，表情变为 DONE。不能立即注入的消息进入队列，在下一个工具边界重试。
- **排队消息。** 排在回合后面的消息按顺序合并成一个回合发给 Agent，每条标注来源和时间。
- **静默回复。** Agent 可以回答 `NO_REPLY`，表示不发任何内容。这个标记不会显示出来，不论在卡片里还是作为消息；重复出现（`NO_REPLYNO_REPLY`）或后面跟着页脚时也一样。
- **自动回合。** 定时提示、其他机器人转来的消息和本地助手 API 的消息不开进度卡。它们在结束时发一条消息；回复为 `NO_REPLY` 则不发。被静音的定时任务只隐藏开始通知，回复和附件照常送达。
- **决策卡**（可选，`[decision] enabled = true`）。Agent 可以发一张带按钮的卡片（`cc-connect send --decision card.json`）。你的点选，或截止时间到后的默认选项，会作为消息回到会话。

其他改动：私聊中每个话题一个会话（可选）、交互卡片和合并转发按文本读取、排队消息在重启后保留、`/stop` 只结束当前回合、`/usage` 显示用量窗口、按模型区分的推理强度、本地助手 API、Codex 后台任务唤醒。完整清单及配置项和源文件，见 [soundworkbench/README.md](soundworkbench/README.md)。

## 配置

步骤列表卡默认关闭。下面的设置可得到上述表现。

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

队列合并默认开启，在顶层的 `[queue_batch]` 段设置。全部配置项和默认值见 [soundworkbench/README.md](soundworkbench/README.md#配置项参考)，带注释的示例见 `config.example.toml`。

## 分支

- `local`：上游发布标签 `v1.5.0` 加本分叉的提交。从这个分支构建。
- `main`：跟随上游，不含本分叉的改动。

## 构建

```sh
soundworkbench/build.sh            # build local
soundworkbench/build.sh <commit>   # build a given commit
```

脚本在 `golang:1.25` Docker 容器中运行有改动的包的测试，测试失败就停止。然后它在 `dist/` 下构建一个 Linux 可执行文件，版本号为 `<上游版本>-sw.<n>`，例如 `v1.5.0-sw.73`。详情见 [soundworkbench/README.md](soundworkbench/README.md#构建)。

## 升级到新的上游版本

先给旧的 `local` 打标签，以便重新构建它，再把 `local` 变基到新的上游标签。步骤和常见冲突见 [soundworkbench/README.md](soundworkbench/README.md#维护-local-分支)。

## 许可证

MIT，与上游相同。见 [LICENSE](LICENSE)。cc-connect 由其上游作者编写，地址 [chenhg5/cc-connect](https://github.com/chenhg5/cc-connect)。

## English

This is a fork of [chenhg5/cc-connect](https://github.com/chenhg5/cc-connect), which connects coding agents such as Claude Code and Codex to chat apps. It runs the upstream release v1.5.0 plus a set of patches, mostly for Feishu/Lark: a streaming progress card, messages that join a running turn, message queues, and scheduled or automated turns. Installation and upstream features are covered in the upstream README. The full list of changes, configuration keys and maintenance steps is in [soundworkbench/README.md](soundworkbench/README.md) (in Chinese).
